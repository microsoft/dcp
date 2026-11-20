/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcptun

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/microsoft/dcp/internal/dcptun/proto"
)

// controlAuthorizationHeader is the gRPC metadata header carrying the bearer token on control RPCs.
const controlAuthorizationHeader = "authorization"

// controlTokenCredentials prevents gRPC from sending the bearer token over an insecure transport.
type controlTokenCredentials struct {
	authorization string
}

func (c controlTokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{
		controlAuthorizationHeader: c.authorization,
	}, nil
}

func (controlTokenCredentials) RequireTransportSecurity() bool {
	return true
}

// ClientDialOptions configures TLS server verification and per-RPC bearer-token authentication for dialing a control server.
func (tc TunnelControlSecurityConfig) ClientDialOptions() ([]grpc.DialOption, error) {
	if validateErr := tc.ValidateClient(); validateErr != nil {
		return nil, validateErr
	}
	clientCertPool, certPoolErr := tc.GetClientPool()
	if certPoolErr != nil {
		return nil, certPoolErr
	}
	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(clientCertPool, "")),
		grpc.WithPerRPCCredentials(controlTokenCredentials{
			authorization: "Bearer " + tc.OutgoingToken,
		}),
	}, nil
}

// ServerOptions configures TLS and bearer-token verification for all unary and streaming RPCs on a control server.
func (tc TunnelControlSecurityConfig) ServerOptions() ([]grpc.ServerOption, error) {
	if validateErr := tc.ValidateServer(); validateErr != nil {
		return nil, validateErr
	}
	tlsConfig, tlsConfigErr := tc.GetServerTLSConfig()
	if tlsConfigErr != nil {
		return nil, tlsConfigErr
	}
	expectedTokenHash, hashDecodeErr := base64.StdEncoding.DecodeString(tc.IncomingTokenHashBase64)
	if hashDecodeErr != nil {
		return nil, fmt.Errorf("decode incoming control token hash: %w", hashDecodeErr)
	}

	authenticate := func(ctx context.Context) error {
		incomingMetadata, found := metadata.FromIncomingContext(ctx)
		if !found {
			return status.Error(codes.Unauthenticated, "control authentication metadata is missing")
		}
		authorizationValues := incomingMetadata.Get(controlAuthorizationHeader)
		if len(authorizationValues) != 1 {
			return status.Error(codes.Unauthenticated, "control authorization header is missing or ambiguous")
		}
		scheme, token, split := strings.Cut(authorizationValues[0], " ")
		if !split || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return status.Error(codes.Unauthenticated, "control authorization header is invalid")
		}
		actualTokenHash := sha256.Sum256([]byte(token))
		if len(expectedTokenHash) != len(actualTokenHash) ||
			subtle.ConstantTimeCompare(expectedTokenHash, actualTokenHash[:]) != 1 {
			return status.Error(codes.Unauthenticated, "control credentials are invalid")
		}
		return nil
	}

	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ChainUnaryInterceptor(func(
			ctx context.Context,
			request any,
			info *grpc.UnaryServerInfo,
			handler grpc.UnaryHandler,
		) (any, error) {
			if authErr := authenticate(ctx); authErr != nil {
				return nil, authErr
			}
			return handler(ctx, request)
		}),
		grpc.ChainStreamInterceptor(func(
			server any,
			stream grpc.ServerStream,
			info *grpc.StreamServerInfo,
			handler grpc.StreamHandler,
		) error {
			if authErr := authenticate(stream.Context()); authErr != nil {
				return authErr
			}
			return handler(server, stream)
		}),
	}, nil
}

// ClientKeepaliveServerOptions returns the keepalive enforcement and idle-probing options
// for the client-side control server, leaving margin for host/VM clock skew and stream teardown.
func ClientKeepaliveServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             ControlKeepaliveMinTime,
			PermitWithoutStream: true,
		}),
		// Probe idle control connections so that a connection whose peer disappeared without
		// closing it cannot keep ownership of the stream-request queue from a replacement connection.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    ControlKeepaliveTime,
			Timeout: ControlKeepaliveTimeout,
		}),
	}
}

// NewControlServer constructs an authenticated gRPC server registered with the given TunnelControl implementation.
func NewControlServer(
	securityConfig TunnelControlSecurityConfig,
	controlServer proto.TunnelControlServer,
	extraOptions ...grpc.ServerOption,
) (*grpc.Server, error) {
	serverOpts, serverOptsErr := securityConfig.ServerOptions()
	if serverOptsErr != nil {
		return nil, serverOptsErr
	}
	serverOpts = append(serverOpts, extraOptions...)
	grpcServer := grpc.NewServer(serverOpts...)
	proto.RegisterTunnelControlServer(grpcServer, controlServer)
	return grpcServer, nil
}
