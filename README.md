# k8s-mutating-webhook — Namespace Velero Scheduler Webhook

This repository contains a Kubernetes mutating/validating webhook that automatically creates Velero schedules and instant backups when Namespaces with specific labels are created or updated. The webhook is implemented in Go and intended to run inside the cluster as an HTTPS server behind Kubernetes admission webhooks.

## Repository layout

- `namespace-webhook/`
  - `main.go` — webhook implementation (HTTP TLS server, `/validate` and `/health` endpoints).
  - `Dockerfile` — container image build for the webhook.
  - `go.mod` / `go.sum` — Go module files.
- `webhook-service.yaml` — Kubernetes Service / Admission configuration (deployment/Service/ValidatingWebhookConfiguration or MutatingWebhookConfiguration — review file for details).
- `webhook-cert-manager.yaml` — cert-manager resources (Issuer/Certificate/Secrets) used to provision TLS certs for the webhook.

> Note: The repository assumes the webhook runs in-cluster and uses the in-cluster kubeconfig to create Velero resources.

## High level behavior

- Listens on port 443 (HTTPS) and expects TLS cert/key mounted at:
  - `/etc/admission-webhook/tls/tls.crt`
  - `/etc/admission-webhook/tls/tls.key`

- Endpoints:
  - `POST /validate` — handles AdmissionReview requests for Namespace create/update/delete.
  - `GET /health` — simple health check returning 200 OK "ok".

- When a Namespace has these labels:
  - `namespace.oam.dev/target: <name>`
  - `usage.oam.dev/runtime: target`

  the webhook will create a Velero Schedule and an instant Backup for that Namespace in the configured Velero namespace.

## Configurable environment variables

The webhook reads these environment variables (defaults shown):

- `VELERO_NAMESPACE` (default: `velero`) — Velero namespace where schedules/backups are created.
- `CRON_EXPRESSION` (default: `@every 1h`) — schedule cron expression.
- `CSI_SNAPSHOT_TIMEOUT` (default: `10m`) — CSI snapshot timeout used in Velero spec.
- `STORAGE_LOCATION` (default: `default`) — Velero storageLocation.
- `BACKUP_TTL` (default: `720h0m0s`) — TTL for backups created by the webhook.
- `DEFAULT_VOLUMES_TO_FS_BACKUP` (default: `true`) — whether to use filesystem backup by default for volumes.
- `BACKUP_SUFFIX` (default: `backup`) — suffix used to name backups/schedules.
- `LOG_FORMAT` (default: `text`) — `json` or `text`.
- `LOG_LEVEL` (default: `info`) — logging level.

## Prerequisites

- Kubernetes cluster (v1.25+ recommended to match client-go used here).
- kubectl configured for the target cluster.
- Docker (or another container builder) to build the image.
- `cert-manager` installed in the cluster if you want automatic certificate issuance (the repo includes `webhook-cert-manager.yaml` as an example).
- Velero installed in the cluster if you want Velero schedules/backups to land somewhere.

## Build (locally)

From repository root you can build the Go binary:

```bash
cd namespace-webhook
# Build binary
go build -o ../bin/namespace-webhook ./...
# or just build the package
go build ./...
```

Or build the container image (example using Docker):

```bash
# from repo root
docker build -t <your-registry>/namespace-webhook:latest -f namespace-webhook/Dockerfile namespace-webhook
# push if needed
docker push <your-registry>/namespace-webhook:latest
```

Notes: The `Dockerfile` context is `namespace-webhook/` so that `main.go` and `go.mod` are correctly available to the build.

## Run locally (development)

The binary expects TLS certs at `/etc/admission-webhook/tls/tls.crt` and `/etc/admission-webhook/tls/tls.key` and binds to port 443. For local testing you can:

1. Generate a self-signed certificate and place it somewhere you control (example using openssl):

```bash
mkdir -p /tmp/admission-webhook/tls
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -subj "/CN=namespace-webhook.default.svc" \
  -keyout /tmp/admission-webhook/tls/tls.key \
  -out /tmp/admission-webhook/tls/tls.crt
```

2. Run the binary with the certs mounted or symlinked to `/etc/admission-webhook/tls/` (requires root to bind 443):

```bash
sudo mkdir -p /etc/admission-webhook/tls
sudo cp /tmp/admission-webhook/tls/tls.* /etc/admission-webhook/tls/
# run the server (requires appropriate kube context if attempting to hit cluster APIs)
sudo ./bin/namespace-webhook
```

Alternatively, run in Docker and mount the certs and use a host network or port mapping.

## Deploy to Kubernetes

1. Ensure the image is built and pushed to a registry accessible by the cluster.
2. Apply or customize `webhook-service.yaml` and `webhook-cert-manager.yaml` to create the necessary Service, Deployment (or Pod), Secret, and webhook configuration. Example:

```bash
kubectl apply -f webhook-cert-manager.yaml
kubectl apply -f webhook-service.yaml
```

3. Confirm that the cert-manager (or manual certificate creation) has produced the TLS secret and that the webhook Service/Deployment pods are healthy.

4. The webhook uses a `ValidatingWebhookConfiguration` / `MutatingWebhookConfiguration` (check `webhook-service.yaml`) that points to the service and expects the server certificate to be signed by the CA used in the webhook configuration. If you use cert-manager, make sure cert-manager updates the CABundle in the webhook config or uses the recommended K8s approach for automatic patching.

## Testing the webhook behavior

Create a namespace with labels that trigger the webhook (replace values as appropriate):

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: example-target-ns
  labels:
    namespace.oam.dev/target: my-target
    usage.oam.dev/runtime: target
```

Apply it:

```bash
kubectl apply -f - <<EOF
<the-namespace-yaml-above>
EOF
```

Then inspect Velero resources (in `VELERO_NAMESPACE`) for a schedule or backup created for `example-target-ns`:

```bash
kubectl get schedules -n $VELERO_NAMESPACE
kubectl get backups -n $VELERO_NAMESPACE
```

Also watch the webhook logs to see the admission events.

## Troubleshooting

- If the webhook calls fail with TLS errors, confirm the service cert is valid and the CA bundle in the webhook configuration matches the issuer.
- If the webhook cannot contact the API server, confirm RBAC and in-cluster config (when running in-cluster `rest.InClusterConfig()` is used).
- Logs are written using logrus; adjust `LOG_LEVEL` and `LOG_FORMAT` to get more details.

## Security notes

- The webhook runs with in-cluster privileges and creates Velero resources—be careful with RBAC scope.
- Use cert-manager or a secure CA to provision TLS certificates, avoid using long-lived self-signed certs in production.
