# Storage lifecycle — auto-expire transient Instagram uploads

Images created for "Criar um post" are uploaded to `instagram/{storeId}/...`,
fetched by Instagram during publish, then deleted by the app. This lifecycle
rule is the **safety net**: it auto-expires anything left under `instagram/`
after 1 day (e.g. an upload whose publish never ran), so storage stays clean
regardless of the app.

## Values you need (from the Railway service variables)

| What | Variable (either name) |
|---|---|
| Bucket | `S3_BUCKET` or `AWS_S3_BUCKET_NAME` |
| Endpoint | `S3_ENDPOINT` or `AWS_ENDPOINT_URL` |
| Region | `AWS_REGION` or `AWS_DEFAULT_REGION` |
| Access key | `AWS_ACCESS_KEY_ID` |
| Secret key | `AWS_SECRET_ACCESS_KEY` |

## Apply (AWS CLI against the S3-compatible endpoint)

Run from this folder. Using `railway run` reuses the app's env vars/credentials:

```bash
railway run aws s3api put-bucket-lifecycle-configuration \
  --endpoint-url "$S3_ENDPOINT" \
  --bucket "$S3_BUCKET" \
  --lifecycle-configuration file://s3-lifecycle.json
```

If you're not using `railway run`, export the 5 vars above first, and use
`AWS_ENDPOINT_URL`/`AWS_S3_BUCKET_NAME` if that's what your project sets.

## Verify

```bash
railway run aws s3api get-bucket-lifecycle-configuration \
  --endpoint-url "$S3_ENDPOINT" --bucket "$S3_BUCKET"
```

You should see the `expire-instagram-uploads` rule.

## Clean up existing orphans (one-time)

```bash
railway run aws s3 rm "s3://$S3_BUCKET/instagram/" --recursive --endpoint-url "$S3_ENDPOINT"
```

These are all transient — the published posts live on Instagram, nothing
references them.

## Notes

- Lifecycle minimum granularity is **1 day**; the app's immediate delete after
  publish handles the happy path, so this rule only catches stragglers.
- Tigris (storageapi.dev / fly.storage.tigris.dev): supports this same S3
  lifecycle API, and you can also set it in the Tigris console (Bucket →
  Lifecycle Rules) with prefix `instagram/`, expire after 1 day.
- Cloudflare R2 / MinIO: same `put-bucket-lifecycle-configuration` call works.
