apply:
	helm upgrade -i cozy-telemetry charts/cozy-telemetry -n cozy-telemetry --create-namespace

diff:
	helm diff upgrade cozy-telemetry charts/cozy-telemetry -n cozy-telemetry

delete:
	helm uninstall cozy-telemetry -n cozy-telemetry

image:
	docker build . -t ghcr.io/aenix-io/cozystack-telemetry-server:latest
