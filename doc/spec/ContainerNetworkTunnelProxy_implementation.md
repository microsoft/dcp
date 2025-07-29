# ContainerNetworkTunnelProxy Implementation Plan

## Phase 1: API and controller skeleton

- [x] Define ContainerNetworkTunnelProxy API and add it to v1 GroupVersion and CleanupResources.
- [x] Implement basic controller watching ContainerNetworkTunnelProxy and ContainerNetwork objects.
- [x] Implement finalizer handling and support for ContainerNetworkTunnelProxy object deletion.
- [x] Implement transitioning to "Running" state when associated ContainerNetwork is in Running state (no proxy functionality yet, just dependency object tracking).
- [x] Write automated (integration) test that creates and deletes a ContainerNetworkTunnelProxy object. The test should verify that the controller adds a finalizer to the object instance and removes it when a deletion is requested.
- [x] Write automated integration test that ensures the ContainerNetworkTunnelProxy object will transition to running even if required ContainerNetwork does not exist initially, or is not running.


## Phase 2: Image build

- [ ] Implement client proxy container image build with associated tagging and version verification, per spec.
- [ ] Ensure that at most one image build runs PER HOST (especially important for test scenarios which tend to run multiple DCP instaces). We have prior art for that with port allocation.


## Phase 3: Proxy instantiation

- [ ] Implement container monitoring capability for `dcpproc`.
- [ ] Implement support for instantiating client proxy container, including exposing ports, reading exposed ports from Docker, and starting `dcpproc` for the newly created container.
- [ ] Implement support for starting server proxy.
- [ ] Implement support for proxy pair cleanup during object deletion.
- [ ] Write automated integration test that verifies the proxy pair has been instantiated by the ContainerNetworkTunnelProxy controller. This might require adding to TestContainerOrchestrator a limited simulation of port allocation and exposure. We are not verifying that the proxy actually works here; just that the server proxy and client proxy (container) are created and hooked up as expected.
- [ ] Write automated integration test that verifies server and client proxies are cleaned up when ContainerNetworkTunnelProxy object is deleted.


## Phase 4: Handling tunnel creation and deletion

- [ ] Implement tunnel preparation and deletion as the Spec.Tunnels set changes.
- [ ] Ensure that TunnelStatuses are updated accordingly, including differentiation between successfully prepared tunnels vs. tunnels that failed.
- [ ] Write automated integration test that verifies the proxies get appropriate tunnel enablement/deletion calls in result to Spec.Tunnels changes.


## Phase 5: Failure handling

- [ ] Ensure that failure of the server side proxy results in transition to Failed state.
- [ ] Ensure that failure of the client side proxy (container) results in transition to failed state.
- [ ] Ensure that transition to failed state results in cleanup of ContainerNetworkTunnelProxy resources and that the Status is updated accordingly.
- [ ] Write automated tests that verify all the above.


## Phase 6: End-to-end testing

- [ ] Implement automated test that launches DCP API server and DCP controllers process and creates a tunnel between a server running on the host (simple echo server will suffice) and a client running as container. The client should verify that it can reach and communicate with the server and should report the result via a REST API that can be called from the test.

    - The test requires a real container orchestrator so it should be considered an "extended" test (only runnable from test-extended Makefile target and not during normal test run).


