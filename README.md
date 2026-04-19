# Swarmex VPA

Vertical Pod Autoscaler — adjusts CPU and RAM limits based on actual usage.

Part of [Swarmex](https://github.com/ccvass/swarmex) — enterprise-grade orchestration for Docker Swarm.

## What It Does

Monitors actual resource consumption of services via Prometheus and right-sizes CPU and memory limits. Prevents over-provisioning by reducing limits for idle services and increases them for resource-starved ones.

## Labels

```yaml
deploy:
  labels:
    swarmex.vpa.enabled: "true"        # Enable vertical autoscaling
    swarmex.vpa.min-memory: "32m"      # Minimum memory limit
    swarmex.vpa.max-memory: "2g"       # Maximum memory limit
    swarmex.vpa.min-cpu: "0.1"         # Minimum CPU limit
    swarmex.vpa.max-cpu: "2.0"         # Maximum CPU limit
```

## How It Works

1. Queries Prometheus for actual CPU and memory usage per service.
2. Calculates recommended limits based on usage patterns with a safety margin.
3. Clamps recommendations within the configured min/max bounds.
4. Updates the service's resource limits via Docker API.
5. Applies changes gradually to avoid disruption.

## Quick Start

```bash
docker service update \
  --label-add swarmex.vpa.enabled=true \
  --label-add swarmex.vpa.min-memory=32m \
  --label-add swarmex.vpa.max-memory=2g \
  my-app
```

## Verified

Idle nginx right-sized from 512M→32M RAM and 1CPU→0.1CPU based on actual usage.

## License

Apache-2.0
