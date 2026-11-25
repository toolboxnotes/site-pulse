Site Pulse 1.3.11 — daily website health check
=============================================

A daily health check for your websites: Google Analytics, Search Console,
AdSense and Bing in one window, explained in plain language. Everything runs
on your own computer.

Website:  https://toolboxnotes.com/
Source, updates and help:  https://github.com/toolboxnotes/site-pulse


FEATURES
- Daily check: is the site up, response time, SSL certificate expiry, domain
  expiry, robots.txt, sitemap, ads.txt
- Google Analytics (GA4), Search Console (sitemap status, indexing of key
  pages), AdSense (account and site status, alerts, policy issues, earnings),
  Bing Webmaster Tools
- Every alert says where it came from (AdSense / Search Console / site
  check…), which site it affects and how to fix it, with a button that opens
  the right console. Alerts that don't apply can be ignored.
- Alerts for: a metric more than 30 % (adjustable) below its 7-day average,
  site down, certificate or domain about to expire, ads.txt errors, pages not
  indexed, expired sign-in, and more
- Compare sites over 7 / 28 / 90 days; Search Console keywords against the
  previous period with daily charts; Google Trends CSV import
- AdSense readiness checklist with pass criteria — print / save as PDF, or
  Excel
- Pages: index tracking with automatic sitemap resubmission, IndexNow,
  broken-link scan, top pages
- Keyword rank tracking, PageSpeed scores, response-time breakdown
- Weekly report every Monday (PDF / Excel)
- Move to another computer: password-encrypted settings export
- Interface in English, Korean and Chinese


RUNNING IT
- Windows: double-click "Site Pulse.exe". It opens in its own Chrome window
  (Edge if Chrome is not installed). Closing the window quits Site Pulse.
  First run: if Windows shows "Windows protected your PC", click
  More info -> Run anyway (the app is not code-signed).
- Mac: unzip Site-Pulse-Mac-v1.3.11.zip and move "Site Pulse.app" to
  Applications. If macOS says the developer cannot be verified: right-click
  -> Open -> Open. If it still won't open: System Settings -> Privacy &
  Security -> Open Anyway.
- Site Pulse only listens on your own computer (127.0.0.1:17380, or the next
  free port). It cannot be reached from outside.


FIRST-TIME SETUP (Settings page)
1. Create your own Google OAuth client (one time, about 10 minutes):
   - Google Cloud Console (https://console.cloud.google.com/) -> new project
   - APIs & Services -> Library: enable Google Analytics Data API, Google
     Analytics Admin API, Google Search Console API, AdSense Management API,
     and (optional) PageSpeed Insights API
   - OAuth consent screen: External; add your own Google account as a test
     user, or publish the app
   - Credentials -> Create credentials -> OAuth client ID -> Desktop app ->
     download the JSON file
   Step-by-step guide:
   https://github.com/toolboxnotes/site-pulse#getting-started
2. Settings -> Google OAuth client -> Choose JSON file
3. Sign in with Google. If Google says "Google hasn't verified this app",
   click Advanced -> Continue — it is your own app.
4. Add your sites -> "Reload lists" and "Auto-match" pick the Search Console
   property, GA4 property, AdSense site and Bing site -> Save
   * If the URL has a path (e.g. https://example.com/ko/), Search Console and
     GA4 count only pages under that path; AdSense and Bing always count the
     whole domain.
5. (Optional) Bing Webmaster Tools -> Settings -> API access -> Generate API
   key, and paste it into Site Pulse
6. (Optional) A PageSpeed Insights API key, if Google limits anonymous
   PageSpeed requests
7. Daily check time and alert threshold: 09:00 and 30 % by default


WHEN THE DAILY CHECK RUNS
- While Site Pulse is open: once a day, after the time you set
- If it was not open at that time: right away the next time you open it
- The first check fetches the last 90 days; later checks re-read the last
  10 days (Search Console data is 2–3 days late)


WHERE YOUR DATA IS KEPT (only on this computer)
- Windows: %APPDATA%\Site Pulse
- Mac: ~/Library/Application Support/Site Pulse
  config.json (settings, including the OAuth client and keys),
  token.json (Google sign-in), metrics.json (daily numbers),
  runs\<date>.json (each day's check), trends.json (imported Trends data),
  Browser-chrome etc. (the app window's own browser profile), log.txt
- To move to another computer: Settings -> Move to another computer (an
  encrypted backup file), or copy this folder.
- To remove everything: delete this folder.


CHANGES
See CHANGELOG.md in the source repository:
https://github.com/toolboxnotes/site-pulse/blob/main/CHANGELOG.md


SOURCE
Go 1.24, standard library only, no third-party dependencies.
https://github.com/toolboxnotes/site-pulse

Copyright (c) 2026 Toolbox Notes. All rights reserved.
Free to use for personal and work purposes.
