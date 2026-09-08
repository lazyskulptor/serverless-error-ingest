# Browser compatibility proof

A minimal page that initializes a **stock** `@sentry/browser` bundle with only
a DSN change, triggers a captured exception, and shows how to verify the wire
contract.

## Files

- `browser/index.html` — the example page (CDN bundle, no build step)

## DSN mapping

Per `docs/COMPATIBILITY.md` §1, the DSN handed to the SDK is:

```
DSN = '{PROTOCOL}://{PUBLIC_KEY}@{HOST}{PATH}/{PROJECT_ID}'
```

which the SDK uses to derive the envelope endpoint:

```
ENDPOINT = '{BASE_URI}/api/{PROJECT_ID}/envelope/'
```

So for a project registered with `scripts/register`:

```sh
cd scripts/register
go run . -project my-app -host <public-host>
```

the page config is:

```js
Sentry.init({
  dsn: "https://<public-key>@<public-host>/my-app",
  tracesSampleRate: 0,
});
```

## How to run it

1. Apply infra and note `public_base_url`. It resolves to either the direct API
   Gateway URL or the Route 53/Cloudflare custom hostname.
2. Register a project (above) and copy the printed DSN into
   `browser/index.html` (`__SENTRY_DSN__`, `__SENTRY_HOST__`,
   `__SENTRY_PROJECT_ID__`).
3. Open the page in a browser (e.g. `python3 -m http.server` in
   `examples/browser`), click **Trigger captured exception**.
4. In DevTools → Network confirm:
   - `POST /api/my-app/envelope/` → `200` with body `{"id": "..."}` (not `202`)
   - one `event` row appears in DynamoDB `sentry-ingest-events`
   - an envelope object appears in S3
     `sentry-ingest-raw-*/projects/my-app/<date>/<event-id>.envelope`

## Verified SDK versions

| SDK | Version | Result |
|---|---|---|
| `@sentry/browser` (CDN bundle) | 7.119.2 | pending live verification |
