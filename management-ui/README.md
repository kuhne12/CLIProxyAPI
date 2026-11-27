# Customized BPS management panel

`build.sh` fetches the management frontend at `upstream-revision`, applies
`bps-quota.patch`, `bps-quota-style.patch`, `credential-concurrency.patch` and
`bps-live-balance.patch` in order, installs its locked dependencies with Bun 1.4.3,
and runs the frontend tests, lint, and production build. The result is one self-contained HTML
file. CI passes this exact artifact and its SHA256 to the deployment script.

```sh
bash management-ui/build.sh /tmp/management.html
```

The patch adds BPS quota windows, localized usage labels, estimated dollar values,
manual reset credit counts, and the existing reset confirmation flow. BPS cards
load automatically, refresh every minute while visible, retain last-good values
on errors, and display observation time and staleness. Accounting and snapshots
are persisted by the plugin; background sampling continues when the panel closes. Unknown
credit counts cannot enable redemption. Reset failures are never automatically
retried. Tests use synthetic credentials and mocked upstream requests; deployment
verification must never consume a production reset credit.

The panel shows BPS estimated remaining and total together for each account
window. The balance disclosure displays the reported amount in dollars and
preserves zero, unavailable and unlimited values. The quota, auth files and
routing pages use the same credential concurrency counts and status colors.

The production image includes the panel. Deployment sets the v8 setting
`management.disable-auto-update-panel` to preserve the customized build. The
previous image and configuration remain available in the deployment backup.

The credential panel polls one authenticated v8 snapshot every five seconds while
visible. Counts cover admitted generation attempts on this CPA instance, including
open streams. An empty file override inherits the default (four for BPS and
unlimited for other providers). Channel-wide and upstream account limits also apply.

The separately deployed usage dashboard uses the installed cpa-usage-stats
0.10.2-custom.3 source as its baseline. Apply usage-stats-custom.4.patch to that
source, run the web tests and build, and build the shared library with version
0.10.2-custom.4. The patch includes the credential email lookup and sub2api
latency thresholds.
