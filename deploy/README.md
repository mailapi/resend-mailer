# Kubernetes deployment

`secret.example.yaml` documents the required Resend API key and client bearer token formats only; Kustomize does not deploy it. Create the real Secret before applying this directory.

```bash
kubectl apply -f namespace.yaml
kubectl -n mailapi create secret generic resend-mailer-secret \
  --from-literal=RESEND_API_KEY=re_123456789 \
  --from-literal=MAILAPI_TOKEN=your-private-bearer-token \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k .
```

## Current limitations

- The 24-hour idempotency journal is stored on `resend-mailer-data` (100 MiB PVC). A default StorageClass is required unless you set one in `pvc.yaml`. Preserve this PVC across restarts. The deployment uses one replica and `Recreate`; a file lock rejects a second writer. High availability needs a shared transactional store. Interrupted executions replay terminal `500` instead of resending.
- Configure the matching bearer token in clients, including `$wgMailAPIToken` for MediaWiki. Health checks remain unauthenticated. Readiness uses `/ready` to test journal-directory writes; liveness uses `/health`.
- Upgrade existing v0.2.x deployments by creating/updating the Secret with `MAILAPI_TOKEN`, provisioning the PVC, and updating clients to accept `202` before rollout.
- The `latest` tag is pulled for each new Pod for development convenience. Pin a release tag or image digest in production.
- The request size is limited to 10 MiB to bound attachment serialization memory usage.
