# Kubernetes deployment

`secret.example.yaml` documents the required value format only; Kustomize does not deploy it. Create the real Secret before applying this directory.

```bash
kubectl apply -f namespace.yaml
kubectl -n mailapi create secret generic resend-mailer-secret \
  --from-literal=RESEND_API_KEY=re_123456789 \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -k .
```

## Current limitations

- The idempotency cache is stored in process memory. The deployment uses one replica to preserve the raw-request idempotency contract while it is running. High availability or cross-restart idempotency requires a shared store with atomic locking, such as Redis.
- The `latest` tag is pulled for each new Pod for development convenience. Pin a release tag or image digest in production.
- The request size is limited to 10 MiB to bound attachment serialization memory usage.
