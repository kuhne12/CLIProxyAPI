# Customized BPS management panel

`build.sh` fetches the management frontend at `upstream-revision`, applies
`bps-quota.patch`, installs its locked dependencies with Bun 1.4.3, and runs the
frontend tests, lint, and production build. The result is one self-contained HTML
file. CI passes this exact artifact and its SHA256 to the deployment script.

```sh
bash management-ui/build.sh /tmp/management.html
```

The patch adds BPS quota windows, localized usage labels, estimated dollar values,
manual reset credit counts, and the existing reset confirmation flow. Unknown
credit counts cannot enable redemption. Reset failures are never automatically
retried. Tests use synthetic credentials and mocked upstream requests; deployment
verification must never consume a production reset credit.

The production image includes the panel. Deployment sets the v8 setting
`management.disable-auto-update-panel` to preserve the customized build. The
previous image and configuration remain available in the deployment backup.
