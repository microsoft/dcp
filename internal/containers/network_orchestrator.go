package containers

import (
	"context"
	"time"

	"github.com/microsoft/usvc-apiserver/internal/pubsub"
)

// CreateNetwork command types

type CreateNetworkOptions struct {
	// Name of the network
	Name string

	// Is IPv6 enabled
	IPv6 bool

	// Labels to apply to the network
	Labels map[string]string
}

type CreateNetwork interface {
	CreateNetwork(ctx context.Context, options CreateNetworkOptions) (string, error)
}

// RemoveNetworks command types

type RemoveNetworksOptions struct {
	// The list of networks to remove
	Networks []string

	// Force removal of the network
	Force bool
}

type RemoveNetworks interface {
	RemoveNetworks(ctx context.Context, options RemoveNetworksOptions) ([]string, error)
}

// InspectNetworks command types

type InspectNetworksOptions struct {
	Networks []string
}

type InspectedNetwork struct {
	// The name of the network
	Name string

	// The ID of the network
	Id string

	// The network driver
	Driver string

	// Labels applied to the network
	Labels map[string]string

	// The network scope
	Scope string

	// True if IPv6 is enabled
	IPv6 bool

	// True if internal network
	Internal bool

	// True if attachable
	Attachable bool

	// True if an ingress network
	Ingress bool

	// Subnets allocated to the network
	Subnets []string

	// Gateways allocated to the network
	Gateways []string

	// IDs of connected containers
	Containers []InspectedNetworkContainer

	// Time the network was created
	CreatedAt time.Time
}

type InspectedNetworkContainer struct {
	// ID of the container
	Id string

	// Name of the container
	Name string
}

type InspectNetworks interface {
	InspectNetworks(ctx context.Context, options InspectNetworksOptions) ([]InspectedNetwork, error)
}

// ConnectNetwork command types

type ConnectNetworkOptions struct {
	// The name or ID of the network to connect to
	Network string

	// The name or ID of a container to connect to the network
	Container string

	// The alias to use for the container on the network
	Aliases []string
}

type ConnectNetwork interface {
	ConnectNetwork(ctx context.Context, options ConnectNetworkOptions) error
}

// DisconnectNetwork command types

type DisconnectNetworkOptions struct {
	// The name or ID of the network to disconnect from
	Network string

	// The name or ID of a container to disconnect from the network
	Container string

	// Force disconnect from the network
	Force bool
}

type DisconnectNetwork interface {
	DisconnectNetwork(ctx context.Context, options DisconnectNetworkOptions) error
}

// ListNetworks command types

type ListedNetwork struct {
	// Driver that created the network
	Driver string

	// Network ID
	ID string

	// True if IPv6 is enabled on the network
	IPv6 bool

	// True if the network is a built-in network
	Internal bool

	// Labels applied to the network
	Labels map[string]string

	// Name of the network
	Name string
}

type ListNetworksFilters struct {
	LabelFilters []LabelFilter
}

type ListNetworksOptions struct {
	Filters ListNetworksFilters
}

type ListNetworks interface {
	ListNetworks(ctx context.Context, options ListNetworksOptions) ([]ListedNetwork, error)
}

type NetworkOrchestrator interface {
	CreateNetwork
	RemoveNetworks
	InspectNetworks
	ConnectNetwork
	DisconnectNetwork
	ListNetworks

	// Subscribes to events about network state changes
	// When the subscription is cancelled, the channel will be closed
	WatchNetworks(sink chan<- EventMessage) (*pubsub.Subscription[EventMessage], error)

	// Get default (bridge-type) network name
	DefaultNetworkName() string

	RuntimeStatusChecker
}
