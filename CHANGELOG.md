# Changelog

## 1.3.9 — 2026-11-17
- Fixed: on Windows the window could stay invisible on the first launch, so Site Pulse had to be opened twice. The browser was started with a "hide window" flag (meant only to avoid a console window), and a freshly started Chrome applied it to its first window. The flag is gone, and a hidden window is now shown anyway.

## 1.3.8 — 2026-11-17
- New: **Settings → Move to another computer.** Save your settings and keys (sites, schedule, ignored alerts, tracked keywords, OAuth client, Bing / PageSpeed / IndexNow keys) to one `.sitepulse` file and open it on the other computer.
  - The file is encrypted with a password you choose (AES-256-GCM, key from PBKDF2-SHA256 with 600,000 rounds). Without the password it cannot be opened, and a modified file is rejected.
  - Optional: include the Google sign-in (off by default — signing in once on the new computer is safer) and the history (on by default).
- Fixed: the first window could open behind other windows, minimized, or off-screen. Once the window connects, Site Pulse restores it and brings it to the front (logged as "window to front: …").

## 1.3.7 — 2026-11-16
- New: **Pages → Google Search Console.** When new pages appear, the daily check resubmits your sitemaps to Google automatically (at most once a day); you can also resubmit by hand.
  - Needs Search Console write permission: after updating, open Pages → Google Search Console and click **Sign in to Google again** once. Everything else stays read-only.
  - Google has no API for "Request indexing" of ordinary pages, so single pages still have to be requested in Search Console.
- Fixed: the "Request indexing" button opened a 404 page (Search Console changed its URL format). The button now copies the page address and opens Search Console — paste it into the search box at the top. There is also **Copy not-indexed addresses**.
- Fixed: the AdSense checklist could take a tag listing (e.g. `/archive/?tag=privacy`) for the privacy policy. Tag, archive and `?` links are now skipped, and URLs containing "privacy" are preferred.
- Fixed: PageSpeed occasionally reported one-off errors such as NO_FCP. It now retries once; if it still fails, the result is shown as information, not a warning.
- Fixed: Search Console and Bing data for the last 2–3 days is not complete yet, which caused false "impressions dropped" alerts. Only complete days are compared now.

## 1.3.6 — 2026-11-15
- Fixed: PageSpeed reported NO_FCP for homepages that redirect by language (the redirect page shows nothing itself). The page after the redirect (e.g. `/en/`) is measured now.
- Fixed: the broken-link scan reported Cloudflare's e-mail protection link (`/cdn-cgi/l/email-protection`) as a 404. A script turns it into an e-mail address in the browser, so it is no longer checked.

## 1.3.5 — 2026-11-12
- Fixed: the window closed by itself right after opening. A hidden window left over from an older version reconnected every 3 seconds and pushed the new window out.
- Every window now gets its own number, and only the most recently opened one counts. Older or hidden windows are told to close and do not reconnect.
- Site Pulse still quits about 5 seconds after you close its window.

## 1.3.4 — 2026-11-12
- The first window could open behind other windows and look like nothing happened; Chrome is now allowed to bring its window to the front.
- Only one window at a time: opening Site Pulse again closes the old window with the note "Site Pulse is open in another window". A forgotten hidden window used to keep Site Pulse running in the background.
- Site Pulse quits about 5 seconds after its window is closed (a running check is finished first).

## 1.3.3 — 2026-11-10
- Homepages that redirect by browser language with JavaScript (e.g. `location.replace("/" + lang + "/")`) are followed via `hreflang` (x-default first), so the real homepage is checked. Previously the redirect page itself was checked, which caused false "no menu / privacy / about / contact links" results.
- The checklist's article sample skips numbered list pages such as `/2/` and `/ko/3/` (they were reported as "too short").
- "Homepage indexed by Google" checks the real homepage after the redirect.

## 1.3.2 — 2026-11-10
- AdSense checklist: every item shows its pass criteria; print / save as PDF or download Excel.
- Privacy / about / contact pages are found even when the homepage redirects inside the page (e.g. Astro's `/` → `/ko/`); the sitemap is used as a fallback.

## 1.3.1
- Quitting when the window is closed now uses a live connection between the window and the app.

## 1.3.0
- AdSense readiness checklist: required pages (privacy / about / contact), number and length of articles, unfinished pages, navigation, mobile viewport, `ads.txt`, indexing… each with ✓ / ✗ and what to fix.
- Pages → Index tracking: finds new pages in the sitemap and checks every day whether Google has indexed them.
- IndexNow: notify Bing, Naver and others about new pages instantly (requires a key file in the site root; the steps are shown in the app).
- Pages → Broken links: weekly scan of all pages and links — 404s, redirect chains and slow pages, with where they are linked from.
- Keywords → Tracked keywords: star a query to record its position every day; alert when it drops 3 places or more.
- Pages → Top pages: by views and Google clicks, with the biggest risers and fallers.
- PageSpeed (mobile / desktop scores, LCP / INP / CLS) and a homepage response-time breakdown (network / server / download).
- Reports: a weekly report every Monday — print / save as PDF, or download Excel.

## 1.2.1
- Alert cards show the source, the affected scope, how to fix it, a button to the right console, and an Ignore button.

## 1.2.0
- Renamed to Site Pulse (the data folder is moved automatically).

## 1.1.1
- Quits when the window is closed; the app window prefers Chrome.

## 1.1.0
- New Compare page: summary cards and a plain-language scoreboard with daily trends.

## 1.0.0
- Daily check: site up, response time, `noindex`, AdSense code, SSL and domain expiry (RDAP), `robots.txt`, sitemap, `ads.txt`.
- Google Analytics 4, Search Console (sitemaps, URL Inspection of key pages), AdSense (account and site status, alerts, policy issues, earnings by site), Bing Webmaster Tools.
- Drop alerts, health alerts, sign-in expiry, desktop notifications.
- Overview, site details, Compare, Keywords (Search Console queries and Google Trends CSV), Settings; history by date.
