//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct{ data string }

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) { return s.data, nil }
func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	mu       sync.Mutex
	releases map[string]*GitHubRelease
	calls    map[string]int
	failures map[string]bool
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[repo]++
	if s.failures[repo] {
		return nil, errors.New("upstream unavailable")
	}
	return s.releases[repo], nil
}
func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	panic("rollback must not fetch")
}
func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("app must not install executable")
}
func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("app must not install executable")
}
func newBudUpdateTest(t *testing.T) (*UpdateService, *updateServiceGitHubClientStub) {
	t.Helper()
	client := &updateServiceGitHubClientStub{calls: map[string]int{}, failures: map[string]bool{}, releases: map[string]*GitHubRelease{
		githubRepo: {TagName: "v0.2.4-Bud.14"}, officialGitHubRepo: {TagName: "v0.2.5"},
	}}
	return NewUpdateService(&updateServiceCacheStub{}, client, "0.2.4-cyberaudit.13", "release"), client
}
func TestBudUpdateVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.2.4-cyberaudit.13", "0.2.4-Bud.14", -1}, {"0.2.4-Bud.9", "0.2.4-Bud.10", -1},
		{"0.2.4-Bud.14", "v0.2.4-Bud.14", 0}, {"0.2.4-Bud.20", "0.2.4-Bud.14", 1},
		{"0.2.4-Bud.99", "0.2.5-Bud.1", -1}, {"0.2.4", "0.2.4", 0},
	} {
		require.Equal(t, tc.want, compareVersions(tc.a, tc.b), "%s / %s", tc.a, tc.b)
	}
	for _, v := range []string{"0.2.4", "0.2.4-Bud.0", "0.2.4-Bud.014", "0.2.4-Bud.14/../x", "0.2.4-Bud.14;id", "0.2.4-Bud.14-rc1", "0.2.4-cyberaudit.15"} {
		require.False(t, validRelease(&GitHubRelease{TagName: v}, githubRepo), v)
	}
}
func TestBudUpdateSourcesAndCache(t *testing.T) {
	svc, client := newBudUpdateTest(t)
	// Legacy cache or another repository cannot become the custom install target.
	svc.cache.(*updateServiceCacheStub).data = `{"latest":"99.0.0","timestamp":9999999999}`
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.2.4-Bud.14", info.LatestVersion)
	require.Equal(t, githubRepo, info.UpdateSource)
	require.True(t, info.Official.HasUpdate)
	require.Equal(t, "0.2.4", info.Official.BaseVersion)
	require.Equal(t, "0.2.5", info.Official.LatestVersion)
	require.Empty(t, info.ReleaseInfo.Assets)
	require.False(t, info.CanUpdate)
	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.Cached)
	require.Equal(t, 1, client.calls[githubRepo])
	require.Equal(t, 1, client.calls[officialGitHubRepo])
	client.failures[githubRepo] = true
	info, err = svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.NotEmpty(t, info.Warning)
	require.Empty(t, info.Official.Warning)
	require.Equal(t, "0.2.4-Bud.14", info.LatestVersion)
	require.ErrorContains(t, svc.PerformUpdate(context.Background()), "release check failed")
}
func TestBudUpdateCannotInstallOfficialOrRollback(t *testing.T) {
	svc, client := newBudUpdateTest(t)
	client.releases[githubRepo] = &GitHubRelease{TagName: "v0.9.9"}
	require.Error(t, svc.PerformUpdate(context.Background()))
	require.ErrorIs(t, svc.Rollback(), ErrRollbackVersionNotAllowed)
	require.ErrorIs(t, svc.RollbackToVersion(context.Background(), "0.2.3"), ErrRollbackVersionNotAllowed)
	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Empty(t, versions)
}
func TestBudUpdateNoUpdateAndIndependentOfficialFailure(t *testing.T) {
	svc, client := newBudUpdateTest(t)
	svc.currentVersion = "0.2.4-Bud.14"
	client.failures[officialGitHubRepo] = true
	require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrNoUpdateAvailable)
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Empty(t, info.Warning)
	require.NotEmpty(t, info.Official.Warning)
	require.False(t, info.HasUpdate)
}
func TestBudUpdateManagedBundleAndIPC(t *testing.T) {
	svc, client := newBudUpdateTest(t)
	dir := t.TempDir()
	svc.updaterSocket = filepath.Join(dir, "u.sock")
	svc.updaterStatus = filepath.Join(dir, "status.json")
	listener, err := net.Listen("unix", svc.updaterSocket)
	require.NoError(t, err)
	defer listener.Close()
	name := "sub2api_0.2.4-Bud.14_linux_amd64.update.tar.gz"
	release := client.releases[githubRepo]
	for _, file := range []string{name, name + ".sig"} {
		release.Assets = append(release.Assets, GitHubAsset{Name: file, Size: 64, BrowserDownloadURL: "https://github.com/" + githubRepo + "/releases/download/" + release.TagName + "/" + file})
	}
	require.True(t, hasManagedBundle(release))
	release.Assets[1].BrowserDownloadURL = "https://github.com/other/project/releases/download/x/" + name + ".sig"
	require.False(t, hasManagedBundle(release))
	release.Assets[1].BrowserDownloadURL = "https://github.com/" + githubRepo + "/releases/download/" + release.TagName + "/" + name + ".sig"
	received := make(chan string, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			received <- ""
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var request map[string]string
		if json.NewDecoder(conn).Decode(&request) != nil || len(request) != 1 {
			received <- ""
			return
		}
		received <- request["version"]
		_, _ = conn.Write([]byte("{\"accepted\":true}\n"))
	}()
	require.NoError(t, svc.PerformUpdate(context.Background()))
	require.Equal(t, "0.2.4-Bud.14", <-received)
	status, err := svc.GetUpdateStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "idle", status.Phase)
	require.NoError(t, os.WriteFile(svc.updaterStatus, []byte(`{"phase":"preparing","version":"0.2.4-Bud.14"}`), 0600))
	status, err = svc.GetUpdateStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, "preparing", status.Phase)
}
