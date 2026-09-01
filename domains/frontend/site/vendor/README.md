# Vendored third-party assets

These files are served from the platform's own S3 bucket / CloudFront distribution
instead of a third-party CDN. Self-hosting is a deliberate decision, for the same
reason the Inter font is self-hosted:

- **Supply chain**: a compromise of a third-party CDN would inject arbitrary
  script into a page that holds an authenticated admin session. Serving from our
  own origin removes that vector entirely.
- **No Subresource Integrity possible**: `cdn.tailwindcss.com` does not return an
  `Access-Control-Allow-Origin` header, and SRI on a cross-origin script requires
  CORS. Adding `integrity` there would make the browser refuse to run the script,
  so pinning by hash was not an option — self-hosting is.
- **Privacy**: no visitor IP is sent to a third party (LGPD/GDPR).

## Contents

| File | Version | Upstream | License |
|---|---|---|---|
| `tailwind-3.4.16.js` | 3.4.16 | `https://cdn.tailwindcss.com/3.4.16` (Tailwind CSS Play CDN build) | MIT — Copyright (c) Tailwind Labs, Inc. |

Chart.js is NOT vendored: `cdn.jsdelivr.net` does return CORS headers, so it is
loaded from the CDN with a Subresource Integrity hash pinned in `console.html`
(`chart.js@4.4.3`, MIT — Copyright (c) Chart.js Contributors).

## Updating

Replace the file with a new pinned version, update the reference in
`site/app/console.html`, the `aws_s3_object` key in `domains/frontend/tf/main.tf`,
and this table. Never point the console at an unversioned CDN URL: the content
would change under us with no review.

sha384 of `tailwind-3.4.16.js` as downloaded:
`sha384-mS5Uq7sE90lgbBDN8xgf34ibEgbZo4gB3tfLY40ZRle+M188BQw8onzNHg6GUZaA`
