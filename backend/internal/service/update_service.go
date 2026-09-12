package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrNoUpdateAvailable         = infraerrors.Conflict("ALREADY_UP_TO_DATE", "no update available; current version is latest")
	ErrRollbackVersionNotAllowed = infraerrors.BadRequest("ROLLBACK_VERSION_NOT_ALLOWED", "Bud releases require a reviewed forward update; online downgrade is disabled")
	budVersionPattern            = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})-(Bud|cyberaudit)\.([1-9][0-9]{0,5})$`)
	officialVersionPattern       = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)
)

const (
	githubRepo         = "Bud668/sub2api"
	officialGitHubRepo = "Wei-Shaw/sub2api"
	updateCacheTTL     = 5 * time.Minute
)

type UpdateCache interface {
	GetUpdateInfo(context.Context) (string, error)
	SetUpdateInfo(context.Context, string, time.Duration) error
}
type GitHubReleaseClient interface {
	FetchLatestRelease(context.Context, string) (*GitHubRelease, error)
	FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error)
	DownloadFile(context.Context, string, string, int64) error
	FetchChecksumFile(context.Context, string) ([]byte, error)
}
type UpdateService struct {
	cache          UpdateCache
	githubClient   GitHubReleaseClient
	currentVersion string
	buildType      string
	checkMu        sync.Mutex
	updaterSocket  string
	updaterStatus  string
}

func NewUpdateService(cache UpdateCache, client GitHubReleaseClient, version, buildType string) *UpdateService {
	return &UpdateService{cache: cache, githubClient: client, currentVersion: version, buildType: buildType,
		updaterSocket: managedUpdaterSocket, updaterStatus: managedUpdaterStatus}
}

type UpdateInfo struct {
	CurrentVersion string              `json:"current_version"`
	LatestVersion  string              `json:"latest_version"`
	HasUpdate      bool                `json:"has_update"`
	ReleaseInfo    *ReleaseInfo        `json:"release_info,omitempty"`
	Cached         bool                `json:"cached"`
	Warning        string              `json:"warning,omitempty"`
	BuildType      string              `json:"build_type"`
	UpdateSource   string              `json:"update_source"`
	CanUpdate      bool                `json:"can_update"`
	Official       *OfficialUpdateInfo `json:"official"`
}

// Official releases are notification-only: no assets or installation target.
type OfficialUpdateInfo struct {
	BaseVersion   string `json:"base_version"`
	LatestVersion string `json:"latest_version"`
	HasUpdate     bool   `json:"has_update"`
	HTMLURL       string `json:"html_url,omitempty"`
	PublishedAt   string `json:"published_at,omitempty"`
	CheckedAt     int64  `json:"checked_at"`
	Warning       string `json:"warning,omitempty"`
}
type ReleaseInfo struct {
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Assets      []Asset `json:"assets,omitempty"`
}
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"download_url"`
	Size        int64  `json:"size"`
}
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []GitHubAsset `json:"assets"`
}
type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}
type RollbackVersion struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}
type cachedRelease struct {
	Repo        string         `json:"repo"`
	Release     *GitHubRelease `json:"release,omitempty"`
	CheckedAt   int64          `json:"checked_at"`
	AttemptedAt int64          `json:"attempted_at"`
	Warning     string         `json:"warning,omitempty"`
}

func (s *UpdateService) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	var sources [2]cachedRelease
	if s.cache != nil {
		if data, err := s.cache.GetUpdateInfo(ctx); err == nil {
			_ = json.Unmarshal([]byte(data), &sources)
		}
	}
	var wg sync.WaitGroup
	cached := true
	for i, repo := range []string{githubRepo, officialGitHubRepo} {
		if sources[i].Repo != repo {
			sources[i] = cachedRelease{Repo: repo}
		}
		age := time.Now().Unix() - sources[i].AttemptedAt
		if !force && age >= 0 && age < int64(updateCacheTTL.Seconds()) {
			continue
		}
		cached = false
		wg.Add(1)
		go func(entry *cachedRelease) {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			release, err := s.githubClient.FetchLatestRelease(fetchCtx, entry.Repo)
			entry.AttemptedAt = time.Now().Unix()
			if err != nil || !validRelease(release, entry.Repo) {
				// Never expose provider errors or discard the other source's result.
				entry.Warning = "Release check failed; retained the last verified result"
				return
			}
			entry.Release, entry.CheckedAt, entry.Warning = release, entry.AttemptedAt, ""
		}(&sources[i])
	}
	wg.Wait()
	if s.cache != nil && !cached {
		data, _ := json.Marshal(sources)
		_ = s.cache.SetUpdateInfo(ctx, string(data), 24*time.Hour)
	}
	base := strings.SplitN(strings.TrimPrefix(s.currentVersion, "v"), "-", 2)[0]
	info := &UpdateInfo{CurrentVersion: s.currentVersion, LatestVersion: s.currentVersion,
		Cached: cached, BuildType: s.buildType, UpdateSource: githubRepo, Warning: sources[0].Warning,
		Official: &OfficialUpdateInfo{BaseVersion: base, CheckedAt: sources[1].CheckedAt, Warning: sources[1].Warning}}
	if r := sources[0].Release; validRelease(r, githubRepo) {
		info.LatestVersion = strings.TrimPrefix(r.TagName, "v")
		info.HasUpdate = compareVersions(s.currentVersion, info.LatestVersion) < 0
		info.ReleaseInfo = &ReleaseInfo{Name: r.Name, Body: r.Body, PublishedAt: r.PublishedAt,
			HTMLURL: "https://github.com/" + githubRepo + "/releases/tag/" + r.TagName}
		info.CanUpdate = s.buildType == "release" && s.updaterAvailable() && hasManagedBundle(r)
	}
	if r := sources[1].Release; validRelease(r, officialGitHubRepo) {
		info.Official.LatestVersion = strings.TrimPrefix(r.TagName, "v")
		info.Official.HasUpdate = compareVersions(base, info.Official.LatestVersion) < 0
		info.Official.HTMLURL = "https://github.com/" + officialGitHubRepo + "/releases/tag/" + r.TagName
		info.Official.PublishedAt = r.PublishedAt
	}
	return info, nil
}
func validRelease(r *GitHubRelease, repo string) bool {
	if r == nil || r.Draft || r.Prerelease {
		return false
	}
	if repo == githubRepo {
		return budVersionPattern.MatchString(r.TagName) && strings.Contains(r.TagName, "-Bud.")
	}
	return repo == officialGitHubRepo && officialVersionPattern.MatchString(r.TagName)
}
func hasManagedBundle(r *GitHubRelease) bool {
	name := "sub2api_" + strings.TrimPrefix(r.TagName, "v") + "_linux_amd64.update.tar.gz"
	found := map[string]bool{}
	for _, asset := range r.Assets {
		if (asset.Name == name || asset.Name == name+".sig") && asset.Size > 0 &&
			asset.BrowserDownloadURL == "https://github.com/"+githubRepo+"/releases/download/"+r.TagName+"/"+asset.Name {
			found[asset.Name] = true
		}
	}
	return found[name] && found[name+".sig"]
}

// The application never replaces its executable or runs a root command. The
// independent, socket-activated installer revalidates and verifies signed assets.
func (s *UpdateService) PerformUpdate(ctx context.Context) error {
	info, err := s.CheckUpdate(ctx, true)
	if err != nil {
		return err
	}
	if info.Warning != "" {
		return fmt.Errorf("Bud release check failed; retry before installing")
	}
	if !info.HasUpdate {
		return ErrNoUpdateAvailable
	}
	if !info.CanUpdate {
		return infraerrors.Conflict("MANAGED_UPDATER_REQUIRED", "Signed Bud installer or compatible update bundle is not available")
	}
	return s.requestManagedUpdate(ctx, info.LatestVersion)
}

// Keep old routes explicit and fail-closed for clients with cached frontend code.
func (s *UpdateService) Rollback() error { return ErrRollbackVersionNotAllowed }
func (s *UpdateService) RollbackToVersion(context.Context, string) error {
	return ErrRollbackVersionNotAllowed
}
func (s *UpdateService) ListRollbackVersions(context.Context) ([]RollbackVersion, error) {
	return []RollbackVersion{}, nil
}

func parseVersion(v string) [4]int {
	result := [4]int{}
	if match := budVersionPattern.FindStringSubmatch(v); match != nil {
		for i, value := range []string{match[1], match[2], match[3], match[5]} {
			result[i], _ = strconv.Atoi(value)
		}
	} else if match := officialVersionPattern.FindStringSubmatch(v); match != nil {
		for i, value := range match[1:] {
			result[i], _ = strconv.Atoi(value)
		}
	}
	return result
}
func compareVersions(current, latest string) int {
	a, b := parseVersion(current), parseVersion(latest)
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
