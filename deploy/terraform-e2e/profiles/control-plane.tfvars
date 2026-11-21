# Control-plane node creation profile. Layered over terraform.tfvars with
# -var-file; see "Profiles" in ../README.md. Own state, own prefix, same topology.
prefix             = "karpenter-e2e-cp"
controller_role_id = "karpenter_e2e_cp_controller"
node_creation_mode = "VIA_CONTROL_PLANE"
additional_pod_ranges = {
  karpenter-e2e-cp-pods-small = "10.13.0.0/22"
  karpenter-e2e-cp-pods-large = "10.14.0.0/20"
}
