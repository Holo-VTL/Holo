# Prometheus Monitoring Setup

This directory contains resources to integrate holo into an existing or new Prometheus observability stack.

## Target Scrape Configuration

To instruct Prometheus to pull metrics from the control-plane, append the following block to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'holo-control-plane'
    scrape_interval: 10s
    metrics_path: '/metrics'
    static_configs:
      - targets: ['localhost:8080'] # Replace with actual holo API host and port
```

## Alert Rules

Alerts defining operational SLO boundaries are maintained in `holo-alerts.yml`. To enable these alerts, add the file path to your Prometheus alert references in `prometheus.yml`:

```yaml
rule_files:
  - "/etc/prometheus/holo-alerts.yml"
```
