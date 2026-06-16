# SystemAdminCredential implementation status

Partial implementation of PLAN.md. Pushed for review. NOT complete, NOT independently built as a full module (workspace had a sparse checkout; only touched packages were compiled).

## Done (compiles in touched packages)
- internal/api/types_system_admin_credential.go — new Cosmos type
- internal/api/registry.go — systemAdminCredentials resource type
- internal/api/types_serviceprovider_cluster.go — ServingCABundle status field
- internal/systemadmincredential/helpers.go — GenerateKeypair, BuildCSR, BuildCSRA, BuildRevocationRequest, BuildRBAC* (x3), BuildKubeconfig, CredName, RevokeOpSuffix (+ unit test)
- internal/database/crud_hcpcluster.go — SystemAdminCredentials CRUD accessor
- Controller 1 dispatch + Controller 2 poll (request) — in operationcontrollers pkg
- Controller 4 dispatch + Controller 5 poll (revoke) — in operationcontrollers pkg
- Controller 9 RevokedGC — in systemadmincredentialcontrollers pkg
- frontend OperationResult — local kubeconfig assembly
- Deleted the 4 old break-glass operation controllers + tests

## Not done / follow-up
- Controllers 3 (IssuanceObserver), 6 (ClusterDeletionCleanup), 7 (PostIssuanceCleanup), 8 (CABundleSync), 10 (ServingCAReadDesireCreator), 11 (DesiresCreator) — all informer-driven; need a SystemAdminCredential informer/lister + desire listers that are not wired yet.
- Controller 11 is required for the request flow to actually produce CSR/CSRA/RBAC ApplyDesires; controller 1 only creates the Cosmos doc. Until 11 exists, no cert is ever requested on the MC.
- Per-credential desire teardown in controller 5 (2 TODO(follow-up) markers).
- backend.go wiring for all new controllers.
- deepcopy/conversion codegen for the new type.
- Removal of now-dead OCM BreakGlass* methods/mocks (cascades to ClusterServiceClientSpec interface).
- Tests for all controllers; integration fixtures.

## Key deviations / assumptions to review
- InternalID: Operation.InternalID is an OCM-SDK type validating cluster-service paths. The plan stores the credential ARM resource ID there, which won't validate. Workaround: flow keys on Spec.OperationID via List; controller 1 synthesizes a CS-style InternalID only to pass validation. Needs a real decision: extend InternalID to accept ARM IDs, or formally key on OperationID.
- Controllers placed in operationcontrollers pkg (not the new pkg) because the GenericOperationController framework helpers are unexported.
- BuildCSR takes the private key (a valid PKCS#10 must be signed); plan signature passed only the public key.
- Assumed default Username "system:admin"; RBAC subject group "system:serviceaccounts:open-cluster-management-agent-addon"; serving-CA secret name still TBD (controller 10).
