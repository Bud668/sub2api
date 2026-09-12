# Custom subscription controls

Based on official v0.2.4. Version 0.2.4-cyberaudit.12 adds the following controls to subscription management.

- **Adjust Dates** remains visible on each card. It changes subscription expiry.
- **More → Convert to admin debug** converts an existing administrator's subscription. Ordinary users cannot receive this exemption. Conversion excludes the subscription from dynamic allocation, preserves its old source binding for billing, and keeps usage, outstanding reservations and receipts. Other subscriptions remain unchanged. Expiry, original group limits, model permissions and safety controls continue to apply. There is no automatic conversion during an upgrade.
- **More → Sync upstream reset** previews the bound source, current cycle and affected active dynamic subscriptions. Opening the preview is a local read. The explicit sync action uses the existing upstream identity, period-boundary and two independent observation checks, at least 30 seconds apart. It can run without the background minute timer; it does not force a reset when evidence is unavailable. A completed cycle cannot be reset again by retrying the same preview. Only subscriptions bound to that source follow its reset. Accounting recovery and old-cycle archival use the existing transaction.
- Group dynamic settings remain in Group Management. The subscription card no longer links to them or exposes the old native daily/weekly/monthly reset action.

The redesigned cards use document scrolling, one row of primary metrics on desktop, and responsive mobile layouts. Allocation time and the next allocation milestone remain adjacent to their values. Debug subscriptions retain a fixed purple border; other borders follow the group.

Reset audit records use the effective V2 cycle consumption, matching the subscription card even when the legacy native weekly counter is lower. Existing billing records and historical audit events are not rewritten.

Regression checks cover source isolation, role restrictions, retained in-flight billing, duplicate operations, independent reset observations and failed quota fetches. Browser checks use synthetic API data across desktop/mobile widths, both languages and themes. Deployment stays on the primary during active development; standby synchronization is deferred until stabilization.
