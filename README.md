# NRP Carbon API

A lightweight Go service that estimates the carbon footprint of LLM inference
on the [National Research Platform (NRP) Nautilus](https://nrp.ai) Kubernetes cluster.

**Live dashboard:** <https://carbon-api.nrp-nautilus.io>

## How it works

1. **GPU power** is read from [NVIDIA DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter)
   metrics already collected by the cluster's Prometheus instance.
2. **Token throughput** is read from vLLM's built-in Prometheus metrics.
3. **Grid carbon intensity** is looked up per node using
   [EPA eGRID 2022](https://www.epa.gov/egrid) subregion averages
   (see `internal/carbon/intensity.go`).

Carbon = Energy x Grid Intensity. See the
[Methodology](https://carbon-api.nrp-nautilus.io/methodology) page for full details.

## Running locally

```bash
export PROMETHEUS_URL=https://prometheus.nrp-nautilus.io
go run ./cmd
# → http://localhost:8080
```

## Deploying to NRP

Build and push the container image, then apply the Kubernetes manifests:

```bash
docker build -t ghcr.io/boettiger-lab/carbon-api:latest .
docker push ghcr.io/boettiger-lab/carbon-api:latest
kubectl apply -f k8s/deployment.yaml
kubectl -n biodiversity rollout restart deployment/carbon-api
```

## API

| Endpoint | Description |
|---|---|
| `GET /api/v1/carbon` | Current metrics for all models |
| `GET /api/v1/carbon/timeseries?range=24h\|7d\|30d` | Cluster-wide CO2 and power time series |
| `GET /api/v1/carbon/{ns}/{container}/{metric}?range=...` | Per-model time series (`power_watts`, `co2_grams_per_hour`, `co2_mg_per_token`) |
| `GET /healthz` | Health check |

## Origin

This project was extracted from the
[boettiger-lab/nautilus](https://github.com/boettiger-lab/nautilus) repository.

## License

[BSD 2-Clause](LICENSE)
