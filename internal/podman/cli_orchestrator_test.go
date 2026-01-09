// Copyright (c) Microsoft Corporation. All rights reserved.

package podman

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
)

func TestInspectedContainerDeserialization(t *testing.T) {
	var b bytes.Buffer

	_, err := b.WriteString(inspectedConsulJune2024)
	require.NoError(t, err)

	inspectedContainers, err := asObjects(&b, unmarshalContainer)
	require.NoError(t, err)
	require.Len(t, inspectedContainers, 1)

	ct := inspectedContainers[0]

	require.Equal(t, "cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1", ct.Id)
	require.Equal(t, "consul-d86ile8", ct.Name)
	require.Equal(t, "docker.io/hashicorp/consul:latest", ct.Image)
	expectedCreatedTime, err := time.Parse(time.RFC3339Nano, "2024-06-14T12:01:02.881754461-07:00")
	require.NoError(t, err)
	require.Equal(t, expectedCreatedTime, ct.CreatedAt)
	expectedStartedTime, err := time.Parse(time.RFC3339Nano, "2024-06-14T12:01:04.283842634-07:00")
	require.NoError(t, err)
	require.Equal(t, expectedStartedTime, ct.StartedAt)
	require.True(t, ct.FinishedAt.IsZero())
	require.Equal(t, containers.ContainerStatusRunning, ct.Status)
	require.EqualValues(t, 0, ct.ExitCode)

	require.Equal(t, containers.InspectedContainerPortMapping{
		// Only the ports that are mapped to the host are included
		"8500/tcp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "39133"}},
		"8600/udp": []containers.InspectedContainerHostPortConfig{{HostIp: "127.0.0.1", HostPort: "39993"}},
	}, ct.Ports)

	require.Equal(t, map[string]string{
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container":       "podman",
		"BIN_NAME":        "consul",
		"PRODUCT_VERSION": "1.19.0",
		"PRODUCT_NAME":    "consul",
		"HOME":            "/root",
		"HOSTNAME":        "cf5947988412",
	}, ct.Env)

	require.Equal(t, []string{"docker-entrypoint.sh", "agent", "-dev", "-client", "0.0.0.0"}, ct.Args)

	require.Equal(t, []containers.InspectedContainerNetwork{
		{
			Id:         "podman",
			Name:       "podman",
			IPAddress:  "10.88.0.8",
			Gateway:    "10.88.0.1",
			MacAddress: "62:c3:4c:66:23:d3",
			Aliases:    []string{"cf5947988412"},
		},
	}, ct.Networks)
}

const inspectedConsulJune2024 = `
[
      {
          "Id": "cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1",
          "Created": "2024-06-14T12:01:02.881754461-07:00",
          "Path": "docker-entrypoint.sh",
          "Args": [
               "agent",
               "-dev",
               "-client",
               "0.0.0.0"
          ],
          "State": {
               "OciVersion": "1.2.0",
               "Status": "running",
               "Running": true,
               "Paused": false,
               "Restarting": false,
               "OOMKilled": false,
               "Dead": false,
               "Pid": 12153,
               "ConmonPid": 12146,
               "ExitCode": 0,
               "Error": "",
               "StartedAt": "2024-06-14T12:01:04.283842634-07:00",
               "FinishedAt": "0001-01-01T00:00:00Z",
               "CgroupPath": "/libpod_parent/libpod-cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1",
               "CheckpointedAt": "0001-01-01T00:00:00Z",
               "RestoredAt": "0001-01-01T00:00:00Z"
          },
          "Image": "bb7114bcaf5225329144303e67841fd4613bd9328d5f0d516db08799b23a7f2a",
          "ImageDigest": "sha256:05baa180b8a505d1bbe60725920544f8ad74bea1f82f45a2c7d89f79b02721ed",
          "ImageName": "docker.io/hashicorp/consul:latest",
          "Rootfs": "",
          "Pod": "",
          "ResolvConfPath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/resolv.conf",
          "HostnamePath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/hostname",
          "HostsPath": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/hosts",
          "StaticDir": "/var/lib/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata",
          "OCIConfigPath": "/var/lib/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/config.json",
          "OCIRuntime": "crun",
          "ConmonPidFile": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/conmon.pid",
          "PidFile": "/run/containers/storage/overlay-containers/cf594798841275880f780c2aa55954b2d218b92a7c82df039aef02e2f4fe83f1/userdata/pidfile",
          "Name": "consul-d86ile8",
          "RestartCount": 0,
          "Driver": "overlay",
          "MountLabel": "",
          "ProcessLabel": "",
          "AppArmorProfile": "",
          "EffectiveCaps": [
               "CAP_CHOWN",
               "CAP_DAC_OVERRIDE",
               "CAP_FOWNER",
               "CAP_FSETID",
               "CAP_KILL",
               "CAP_NET_BIND_SERVICE",
               "CAP_SETFCAP",
               "CAP_SETGID",
               "CAP_SETPCAP",
               "CAP_SETUID",
               "CAP_SYS_CHROOT"
          ],
          "BoundingCaps": [
               "CAP_CHOWN",
               "CAP_DAC_OVERRIDE",
               "CAP_FOWNER",
               "CAP_FSETID",
               "CAP_KILL",
               "CAP_NET_BIND_SERVICE",
               "CAP_SETFCAP",
               "CAP_SETGID",
               "CAP_SETPCAP",
               "CAP_SETUID",
               "CAP_SYS_CHROOT"
          ],
          "ExecIDs": [],
          "GraphDriver": {
               "Name": "overlay",
               "Data": {
                    "LowerDir": "/var/lib/containers/storage/overlay/0ebba15ea4e7eda05faa9f8e199f128f0218dc704b76189e79ab8e45be1684b4/diff:/var/lib/containers/storage/overlay/ad789f5ddef99a9c34b66a238fe4432f66961206e6a4689473554df9d3d441c6/diff:/var/lib/containers/storage/overlay/4eeb1a474633de44192a9cbdf44b43881064119529a569c550226ab2aabcae7c/diff:/var/lib/containers/storage/overlay/f0dd272e72bca22d3b58721b0451d43533527eb69c8022f69e878207650a4417/diff:/var/lib/containers/storage/overlay/08c74feafbfd0ed9b5fd08b120e888fa06c78f36432fa67af4eeb99259e2799c/diff:/var/lib/containers/storage/overlay/f15694bde2ada692fe0faa088896d6b06028d1fe80caa086350df9be15d9b37b/diff:/var/lib/containers/storage/overlay/bc29d22c7eb7c1d866c31f66defac8af9642964c77f3f1c1e8a2af33b55b24b5/diff:/var/lib/containers/storage/overlay/5fc209161df7fe8003405fb262d556af088c26785b99e102d9bcb653f0915f10/diff:/var/lib/containers/storage/overlay/d4fc045c9e3a848011de66f34b81f052d4f2c15a17bb196d637e526349601820/diff",
                    "MergedDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/merged",
                    "UpperDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/diff",
                    "WorkDir": "/var/lib/containers/storage/overlay/fa0c642180310ba359bf2dc3c1501d3620083b4928d14585a3e45145e7727ce6/work"
               }
          },
          "Mounts": [
               {
                    "Type": "volume",
                    "Name": "68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a",
                    "Source": "/var/lib/containers/storage/volumes/68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a/_data",
                    "Destination": "/consul/data",
                    "Driver": "local",
                    "Mode": "",
                    "Options": [
                         "nodev",
                         "exec",
                         "nosuid",
                         "rbind"
                    ],
                    "RW": true,
                    "Propagation": "rprivate"
               }
          ],
          "Dependencies": [],
          "NetworkSettings": {
               "EndpointID": "",
               "Gateway": "10.88.0.1",
               "IPAddress": "10.88.0.8",
               "IPPrefixLen": 16,
               "IPv6Gateway": "",
               "GlobalIPv6Address": "",
               "GlobalIPv6PrefixLen": 0,
               "MacAddress": "62:c3:4c:66:23:d3",
               "Bridge": "",
               "SandboxID": "",
               "HairpinMode": false,
               "LinkLocalIPv6Address": "",
               "LinkLocalIPv6PrefixLen": 0,
               "Ports": {
                    "8300/tcp": null,
                    "8301/tcp": null,
                    "8301/udp": null,
                    "8302/tcp": null,
                    "8302/udp": null,
                    "8500/tcp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39133"
                         }
                    ],
                    "8600/tcp": null,
                    "8600/udp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39993"
                         }
                    ]
               },
               "SandboxKey": "/run/netns/netns-802a14fb-7d36-bf94-60d9-ec07a6bf9560",
               "Networks": {
                    "podman": {
                         "EndpointID": "",
                         "Gateway": "10.88.0.1",
                         "IPAddress": "10.88.0.8",
                         "IPPrefixLen": 16,
                         "IPv6Gateway": "",
                         "GlobalIPv6Address": "",
                         "GlobalIPv6PrefixLen": 0,
                         "MacAddress": "62:c3:4c:66:23:d3",
                         "NetworkID": "podman",
                         "DriverOpts": null,
                         "IPAMConfig": null,
                         "Links": null,
                         "Aliases": [
                              "cf5947988412"
                         ]
                    }
               }
          },
          "Namespace": "",
          "IsInfra": false,
          "IsService": false,
          "KubeExitCodePropagation": "invalid",
          "lockNumber": 9,
          "Config": {
               "Hostname": "cf5947988412",
               "Domainname": "",
               "User": "",
               "AttachStdin": false,
               "AttachStdout": false,
               "AttachStderr": false,
               "Tty": false,
               "OpenStdin": false,
               "StdinOnce": false,
               "Env": [
                    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                    "container=podman",
                    "BIN_NAME=consul",
                    "PRODUCT_VERSION=1.19.0",
                    "PRODUCT_NAME=consul",
                    "HOME=/root",
                    "HOSTNAME=cf5947988412"
               ],
               "Cmd": [
                    "agent",
                    "-dev",
                    "-client",
                    "0.0.0.0"
               ],
               "Image": "docker.io/hashicorp/consul:latest",
               "Volumes": null,
               "WorkingDir": "/",
               "Entrypoint": [
                    "docker-entrypoint.sh"
               ],
               "OnBuild": null,
               "Labels": {
                    "org.opencontainers.image.authors": "Consul Team \u003cconsul@hashicorp.com\u003e",
                    "org.opencontainers.image.description": "Consul is a datacenter runtime that provides service discovery, configuration, and orchestration.",
                    "org.opencontainers.image.documentation": "https://www.consul.io/docs",
                    "org.opencontainers.image.licenses": "BSL-1.1",
                    "org.opencontainers.image.source": "https://github.com/hashicorp/consul",
                    "org.opencontainers.image.title": "consul",
                    "org.opencontainers.image.url": "https://www.consul.io/",
                    "org.opencontainers.image.vendor": "HashiCorp",
                    "org.opencontainers.image.version": "1.19.0",
                    "version": "1.19.0"
               },
               "Annotations": {
                    "io.container.manager": "libpod",
                    "org.opencontainers.image.stopSignal": "15"
               },
               "StopSignal": "SIGTERM",
               "HealthcheckOnFailureAction": "none",
               "CreateCommand": [
                    "C:\\Users\\karolz\\scoop\\apps\\podman\\current\\podman.exe",
                    "create",
                    "--name",
                    "consul-d86ile8",
                    "-p",
                    "127.0.0.1::8500/TCP",
                    "-p",
                    "127.0.0.1::8600/UDP",
                    "docker.io/hashicorp/consul:latest"
               ],
               "Umask": "0022",
               "Timeout": 0,
               "StopTimeout": 10,
               "Passwd": true,
               "sdNotifyMode": "container"
          },
          "HostConfig": {
               "Binds": [
                    "68f459c21291ee911e70222b661c6c667b80ee1bb6fa3dcd39fb1181fe4caf7a:/consul/data:rprivate,rw,nodev,exec,nosuid,rbind"
               ],
               "CgroupManager": "cgroupfs",
               "CgroupMode": "host",
               "ContainerIDFile": "",
               "LogConfig": {
                    "Type": "journald",
                    "Config": null,
                    "Path": "",
                    "Tag": "",
                    "Size": "0B"
               },
               "NetworkMode": "bridge",
               "PortBindings": {
                    "8500/tcp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39133"
                         }
                    ],
                    "8600/udp": [
                         {
                              "HostIp": "127.0.0.1",
                              "HostPort": "39993"
                         }
                    ]
               },
               "RestartPolicy": {
                    "Name": "no",
                    "MaximumRetryCount": 0
               },
               "AutoRemove": false,
               "Annotations": {
                    "io.container.manager": "libpod",
                    "org.opencontainers.image.stopSignal": "15"
               },
               "VolumeDriver": "",
               "VolumesFrom": null,
               "CapAdd": [],
               "CapDrop": [],
               "Dns": [],
               "DnsOptions": [],
               "DnsSearch": [],
               "ExtraHosts": [],
               "GroupAdd": [],
               "IpcMode": "shareable",
               "Cgroup": "",
               "Cgroups": "default",
               "Links": null,
               "OomScoreAdj": 0,
               "PidMode": "private",
               "Privileged": false,
               "PublishAllPorts": false,
               "ReadonlyRootfs": false,
               "SecurityOpt": [],
               "Tmpfs": {},
               "UTSMode": "private",
               "UsernsMode": "",
               "ShmSize": 65536000,
               "Runtime": "oci",
               "ConsoleSize": [
                    0,
                    0
               ],
               "Isolation": "",
               "CpuShares": 0,
               "Memory": 0,
               "NanoCpus": 0,
               "CgroupParent": "",
               "BlkioWeight": 0,
               "BlkioWeightDevice": null,
               "BlkioDeviceReadBps": null,
               "BlkioDeviceWriteBps": null,
               "BlkioDeviceReadIOps": null,
               "BlkioDeviceWriteIOps": null,
               "CpuPeriod": 0,
               "CpuQuota": 0,
               "CpuRealtimePeriod": 0,
               "CpuRealtimeRuntime": 0,
               "CpusetCpus": "",
               "CpusetMems": "",
               "Devices": [],
               "DiskQuota": 0,
               "KernelMemory": 0,
               "MemoryReservation": 0,
               "MemorySwap": 0,
               "MemorySwappiness": 0,
               "OomKillDisable": false,
               "PidsLimit": 2048,
               "Ulimits": [
                    {
                         "Name": "RLIMIT_NPROC",
                         "Soft": 4194304,
                         "Hard": 4194304
                    }
               ],
               "CpuCount": 0,
               "CpuPercent": 0,
               "IOMaximumIOps": 0,
               "IOMaximumBandwidth": 0,
               "CgroupConf": null
          }
     }
]`
