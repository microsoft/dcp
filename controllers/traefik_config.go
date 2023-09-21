// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"fmt"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

func NewTcpProxyConfig(svc *apiv1.Service, endpoints *apiv1.EndpointList) tcpProxyConfig {
	addressList := getAddressListForEndpoints(endpoints)
	serviceName := svc.ObjectMeta.Name

	return tcpProxyConfig{
		Tcp: tcpConfig{
			Routers: map[string]tcpRouterConfig{
				serviceName: {
					EntryPoints: []string{"web", "webipv6"},
					Service:     serviceName,
					Rule:        "HostSNI(`*`)",
				},
			},
			Services: map[string]serviceConfig{
				serviceName: {
					LoadBalancer: loadBalancerConfig{
						Servers: slices.Map[string, serverConfig](addressList, func(address string) serverConfig {
							return serverConfig{
								Address: address,
							}
						}),
					},
				},
			},
		},
	}
}

func NewUdpProxyConfig(svc *apiv1.Service, endpoints *apiv1.EndpointList) udpProxyConfig {
	addressList := getAddressListForEndpoints(endpoints)
	serviceName := svc.ObjectMeta.Name

	return udpProxyConfig{
		Udp: udpConfig{
			Routers: map[string]udpRouterConfig{
				serviceName: {
					EntryPoints: []string{"web", "webipv6"},
					Service:     serviceName,
				},
			},
			Services: map[string]serviceConfig{
				serviceName: {
					LoadBalancer: loadBalancerConfig{
						Servers: slices.Map[string, serverConfig](addressList, func(address string) serverConfig {
							return serverConfig{
								Address: address,
							}
						}),
					},
				},
			},
		},
	}
}

func getAddressListForEndpoints(endpoints *apiv1.EndpointList) []string {
	if endpoints.ItemCount() == 0 {
		// Send to black hole address so the server throws 502 Bad Gateway
		return []string{"0.0.0.0:0"}
	} else {
		return slices.Map[apiv1.Endpoint, string](endpoints.Items, func(endpoint apiv1.Endpoint) string {
			return fmt.Sprintf("%s:%d", endpoint.Spec.Address, endpoint.Spec.Port)
		})
	}
}

type tcpProxyConfig struct {
	Tcp tcpConfig `yaml:"tcp"`
}

type tcpConfig struct {
	Routers  map[string]tcpRouterConfig `yaml:"routers"`
	Services map[string]serviceConfig   `yaml:"services"`
}

type tcpRouterConfig struct {
	EntryPoints []string `yaml:"entryPoints"`
	Rule        string   `yaml:"rule"`
	Service     string   `yaml:"service"`
}

type serviceConfig struct {
	LoadBalancer loadBalancerConfig `yaml:"loadBalancer"`
}

type loadBalancerConfig struct {
	Servers []serverConfig `yaml:"servers"`
}

type serverConfig struct {
	Address string `yaml:"address"`
}

type udpProxyConfig struct {
	Udp udpConfig `yaml:"udp"`
}

type udpConfig struct {
	Routers  map[string]udpRouterConfig `yaml:"routers"`
	Services map[string]serviceConfig   `yaml:"services"`
}

type udpRouterConfig struct {
	EntryPoints []string `yaml:"entryPoints"`
	Service     string   `yaml:"service"`
}
