# ContainerNetworkTunnelProxy Implementation Plan

## Phase 1: API and controller skeleton (complete)

- [x] Define ContainerNetworkTunnelProxy API and add it to v1 GroupVersion and CleanupResources.
- [x] Implement basic controller watching ContainerNetworkTunnelProxy and ContainerNetwork objects.
- [x] Implement finalizer handling and support for ContainerNetworkTunnelProxy object deletion.
- [x] Implement transitioning to "Running" state when associated ContainerNetwork is in Running state (no proxy functionality yet, just dependency object tracking).
- [x] Write automated (integration) test that creates and deletes a ContainerNetworkTunnelProxy object. The test should verify that the controller adds a finalizer to the object instance and removes it when a deletion is requested.
- [x] Write automated integration test that ensures the ContainerNetworkTunnelProxy object will transition to running even if required ContainerNetwork does not exist initially, or is not running.


## Phase 2: Image build (complete)

- [x] Implement client proxy container image build with associated tagging and version verification, per spec.
- [x] Ensure that at most one image build runs PER HOST (especially important for test scenarios which tend to run multiple DCP instances). We have prior art for that with port allocation.


## Phase 3: Proxy instantiation

- [x] Implement container monitoring capability for `dcpproc`.
- [x] Implement support for instantiating client proxy container, including exposing ports, reading exposed ports from Docker, and starting `dcpproc` for the newly created container.
- [x] Implement support for starting server proxy.
- [x] Implement support for proxy pair cleanup during object deletion.
- [x] Write automated integration test that verifies the proxy pair has been instantiated by the ContainerNetworkTunnelProxy controller. This might require adding to TestContainerOrchestrator a limited simulation of port allocation and exposure. We are not verifying that the proxy actually works here; just that the server proxy and client proxy (container) are created and hooked up as expected.
- [x] Write automated integration test that verifies server and client proxies are cleaned up when ContainerNetworkTunnelProxy object is deleted.


## Phase 4: Handling tunnel creation and deletion

- [x] Modify TunnelConfiguration definition so that both the server and the client are identified by a Service object instead of explicit address and port combination. This means ServiceAddress, ServicePort, ClientProxyAddress, and ClientProxyPort properties should be deleted and replaced by server Service name and namespace and client proxy Service name and namespace.
- [x] Make the ContainerNetworkTunnelProxyReconciler watches Services.
- [x] Implement tunnel preparation and deletion as the Spec.Tunnels set changes. Tunnel cannot be prepared until server and client Services exist and the server Service is in Ready state.
- [x] Ensure that TunnelStatuses are updated accordingly, including differentiation between successfully prepared tunnels vs. tunnels that failed.
- [x] Write automated integration test that verifies the proxies get appropriate tunnel enablement/deletion calls in result to Spec.Tunnels changes.
- [x] Write automated integration test that verifies tunnels get prepared/disabled depending on server Service being in Ready vs NotReady state.
- [x] Write automated integration test that verifies the controller creates and deletes Endpoints that are associated with client Service (for each tunnel).

## Phase 5: Support for DNS names ("aliases") for container-side proxy.

- [x] Add facility for specifying multiple aliases for a new container in generic container orchestrator interfaces.
- [x] Implement support for container aliases in Docker and Podman container orchestrators.
- [x] Modify ContainerNetworkTunnelProxy spec to include ability to specify aliases for client-side (container) proxy.
- [x] Change ContainerNetworkTunnelProxy controller so that it uses aliases in the spec when creating client-side proxy container.
- [x] Write automated integration tests that verifies the controller applies aliases to client-side proxy container when requested by ContainerNetworkTunnelProxy spec.

## Phase 6: Failure handling

- [x] Ensure that failure of the server side proxy results in transition to Failed state.
- [x] Ensure that failure of the client side proxy (container) results in transition to Failed state.
- [x] Ensure that transition to failed state results in cleanup of ContainerNetworkTunnelProxy resources and that the Status is updated accordingly.
- [x] Ensure that tunnels are shut down if their server Service goes from Ready to NotReady (and vice versa).
- [x] When ContainerNetworkTunnelProxy transitions to Failed state, all existing tunnels should be marked as failed too.

## Phase 7 Observability (maybe skip)

- [ ] Implement system log subresource for ContainerNetworkTunnelProxy.
- [ ] Ensure that important lifetime changes for ContainerNetworkTunnelProxy (e.g. results of precondition checks for transitioning out of Pending state) end up logged into the system log.


## Phase 8: End-to-end testing

- [ ] Implement automated test that launches DCP API server and DCP controllers process and creates a tunnel between a server running on the host (simple echo server will suffice) and a client running as container. The client should verify that it can reach and communicate with the server and should report the result via a REST API that can be called from the test.

    - The test requires a real container orchestrator so it should be considered an "extended" test (only runnable from test-extended Makefile target and not during normal test run).

## Phase 9: security

- [ ] Make sure that ContainerNetworkTunnelProxy is using secure endpoints for control connections. This probably means auto-generated certificates, just like we do for kubeconfig files.
