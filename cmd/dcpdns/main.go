/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// dcpdns is the DNS forwarder DCP runs inside a container on the Apple container runtime's
// network to provide name resolution for container names and network aliases.
// See internal/dcpdns for details.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-logr/logr/funcr"

	"github.com/microsoft/dcp/internal/dcpdns"
)

func main() {
	listenAddr := flag.String("listen", ":53", "Address to serve DNS on")
	hostsPath := flag.String("hosts", "", "Path to the hosts-format records file (required)")
	upstream := flag.String("upstream", "", "Upstream resolver as host[:port]; defaults to the first nameserver in /etc/resolv.conf")
	verbose := flag.Bool("v", false, "Enable verbose logging")
	flag.Parse()

	log := funcr.New(func(prefix, args string) {
		fmt.Fprintln(os.Stdout, prefix, args)
	}, funcr.Options{Verbosity: verbosity(*verbose)})

	if *hostsPath == "" {
		log.Error(nil, "--hosts is required")
		os.Exit(2)
	}

	upstreamAddr := *upstream
	if upstreamAddr == "" {
		resolvConfUpstream, resolvErr := upstreamFromResolvConf("/etc/resolv.conf")
		if resolvErr != nil {
			log.Error(resolvErr, "No --upstream specified and no nameserver found in /etc/resolv.conf")
			os.Exit(2)
		}
		upstreamAddr = resolvConfUpstream
	}
	if _, _, splitErr := net.SplitHostPort(upstreamAddr); splitErr != nil {
		upstreamAddr = net.JoinHostPort(upstreamAddr, "53")
	}

	udpConn, udpErr := net.ListenPacket("udp", *listenAddr)
	if udpErr != nil {
		log.Error(udpErr, "Failed to listen for UDP DNS queries", "Address", *listenAddr)
		os.Exit(1)
	}

	tcpListener, tcpErr := net.Listen("tcp", *listenAddr)
	if tcpErr != nil {
		log.Error(tcpErr, "Failed to listen for TCP DNS queries", "Address", *listenAddr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("DCP DNS forwarder starting", "Listen", *listenAddr, "Upstream", upstreamAddr, "Hosts", *hostsPath)

	server := dcpdns.NewServer(dcpdns.NewRecords(*hostsPath), upstreamAddr, log)
	if serveErr := server.Serve(ctx, udpConn, tcpListener); serveErr != nil {
		log.Error(serveErr, "DNS forwarder failed")
		os.Exit(1)
	}
}

func verbosity(verbose bool) int {
	if verbose {
		return 2
	}
	return 0
}

// upstreamFromResolvConf returns the first nameserver listed in the given resolv.conf file.
func upstreamFromResolvConf(path string) (string, error) {
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", readErr
	}

	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			return fields[1], nil
		}
	}

	return "", fmt.Errorf("no nameserver entries in %s", path)
}
