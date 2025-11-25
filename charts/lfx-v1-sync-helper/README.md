# LFX v1 Sync Helper Helm Chart

This Helm chart deploys the LFX v1 Sync Helper service, which monitors NATS KV stores for v1 data and synchronizes it with the LFX v2 platform APIs, handling data transformation and conflict resolution.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.2.0+
- LFX v2 platform deployed (with NATS and required APIs)
- Access to LFX v1 data sources via Meltano pipeline

## Installing the chart

### Installing from source

Clone the repository before running the following commands from the root of the working directory.

```bash
# Create namespace (recommended)
kubectl create namespace lfx

# Install the chart
helm install -n lfx lfx-v1-sync-helper \
    ./charts/lfx-v1-sync-helper
```

## Uninstalling the chart

To uninstall/delete the `lfx-v1-sync-helper` deployment:

```bash
helm uninstall lfx-v1-sync-helper -n lfx
```

## Configuration

For all available configuration options and their default values, please see the [values.yaml](values.yaml) file in this chart directory. You can override these values in your own `values.yaml` file or by using the `--set` flag when installing the chart.

