# DCP API server (dcpd)
dcpd is the Developer Control Plane API server that holds the workload model. It is used by controllers, workload renderers, and API providers to create workload definition, run it, and expose information about it.

`dcpd` is Kubernetes-compatible. It uses [Tilt API server library](https://github.com/tilt-dev/tilt-apiserver), which is built on top of standard Kubernetes libraries.

## Development environment setup
You will need Go 1.19 or newer. Supported operating systems are Linux and MacOS; Windows is not supported at this time. 

## Running `dcpd`

To start DCP API server run

```shell
go run ./cmd/dcpd/main.go --secure-port=9562 --token=outdoor-salad
```

To connect to the API server using `kubectl` and similar Kubernetes tools use the kubeconfig file in the root of this repository. For example:

```shell
kubectl --kubeconfig ./kubeconfig api-resources
```

