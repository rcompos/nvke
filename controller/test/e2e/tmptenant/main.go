// Command tmptenant prints the CreateClusterRequest a DSX tenant with one
// control-plane node and no workers would be created from.
//
// It is the same construction nicoDSXSpec makes, built from the same
// package, so the payloads are the qualification's own rather than
// hand-written JSON. TEMPORARY: delete with the manual run it exists for.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/nvke"
)

func main() {
	const (
		orgID          = "ncx"
		siteID         = "22f169c6-2e67-4b91-8558-208fe1777047"
		vpcID          = "01a133ba-9b18-7d78-b2e3-fce79a00aba7"
		vpcPrefixID    = "9ac513b7-fba0-4cb7-9212-2b2fe5f4b84f"
		sshKeyGroupID  = "f411a62c-cd9c-4a10-b2ff-a0d18d6547fa"
		instanceTypeID = "f3505a1b-ba6b-4f71-ad4a-8afc9d48b280"

		kubernetesVersion = "1.36.3"
		cniVersion        = "2.4.2"
		arch              = "arm64"

		bootLoaderURL = "https://d1veq6e8nuk2fy.cloudfront.net/nke-isos/qcow/efi/aarch64/2026-06-25/1782410719/qcow-imager.efi"
		imageURL      = "https://d3stn69qht3x25.cloudfront.net/nvke-packages/os-image/releases/sha-711387ca29fa6dba1c0c39847f001bb2af96f772/arm64/nvke-node-non-fde-ubuntu-24.04.5-arm64.qcow2"
	)

	// AgentArgs left empty, as forgeRolePayloads does: the default expects a
	// SPIFFE identity, which is right for a site that installed SPIRE.
	payload, err := nvke.BuildForgeNodePayloads(nvke.NodeInputs{
		BootLoaderURL:     bootLoaderURL,
		ImageURL:          imageURL,
		Arch:              arch,
		KubernetesVersion: kubernetesVersion,
		CNIVersion:        cniVersion,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "payloads:", err)
		os.Exit(1)
	}

	spec := nvke.ClusterSpec{
		Name:              "manual-dsx",
		Backend:           nvke.BackendNicoForge,
		KubernetesVersion: kubernetesVersion,
		CNIPlugin:         "ovn-kubernetes",
		CNIVersion:        cniVersion,
		OrgID:             orgID,
		SiteID:            siteID,
		VpcID:             vpcID,
		// Both roles are the same arch here, so the same payload twice.
		NodePayloads: &nvke.RolePayloads{ControlPlane: payload, Worker: payload},
		ControlPlane: nvke.ControlPlane{
			// No control-plane pools: the controller refuses them and wants the
			// cluster-level controlPlaneConfig instead. nicoDSXTenant leaves
			// shape.controlPlanePools empty for the same reason.
			Config: &nvke.ControlPlaneConfigSpec{
				DesiredCount:         1,
				SystemReservedMemory: "16Gi",
				InstanceTypeID:       instanceTypeID,
				VpcPrefixIDs:         []string{vpcPrefixID},
				SSHKeyGroupIDs:       []string{sshKeyGroupID},
			},
			// Empty: each instance consumes a /31 and lands on the odd
			// address, so the prefix has no spare address for a VIP.
			ControlPlaneVIP: "",
		},
		// TEMPORARY, as on the branch: no worker pool, so the run needs two
		// trays rather than three.
		Workers: nil,
	}

	req, err := nvke.BuildCreateRequest(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build request:", err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
