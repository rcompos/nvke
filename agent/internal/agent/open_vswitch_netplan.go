// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	pb "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"
)

const openVSwitchNetplanDir = "/etc/netplan"

// openVSwitchNetplanDocument holds validated views into a parsed netplan tree.
// Maps are intentional because netplan is extensible and unknown fields must
// round-trip unchanged. Mutating ethernets or bridges updates root directly.
type openVSwitchNetplanDocument struct {
	root      map[string]any
	ethernets map[string]any
	bridges   map[string]any
}

type openVSwitchNetplanCandidate struct {
	path       string
	document   openVSwitchNetplanDocument
	uplinkName string
	pending    bool
}

type openVSwitchNetplanFile struct {
	path string
	data []byte
}

// Two netplan entries can configure one NIC, as on an image that ships a
// DHCP-on-every-NIC file beside the one cloud-init writes. They are merged
// only when their addressing is the same; the most specific one is bridged.
const (
	openVSwitchNetplanMatchesByName = iota + 1
	openVSwitchNetplanMatchesByMAC
	openVSwitchNetplanNamesUplink
	openVSwitchNetplanBridgesUplink
)

func (c openVSwitchNetplanCandidate) specificity(link openVSwitchLink) int {
	switch {
	case c.pending:
		return openVSwitchNetplanBridgesUplink
	case c.uplinkName == link.Name:
		return openVSwitchNetplanNamesUplink
	}
	stanza, _ := c.document.ethernets[c.uplinkName].(map[string]any)
	match, _ := stanza["match"].(map[string]any)
	if mac, _ := match["macaddress"].(string); link.Address != "" && strings.EqualFold(mac, link.Address) {
		return openVSwitchNetplanMatchesByMAC
	}
	return openVSwitchNetplanMatchesByName
}

// addressing is the candidate's stanza without the keys that only identify
// the NIC or attach it to the bridge, so entries for one NIC can be compared.
func (c openVSwitchNetplanCandidate) addressing(bridgeName string) map[string]any {
	raw := c.document.ethernets[c.uplinkName]
	if c.pending {
		raw = c.document.bridges[bridgeName]
	}
	stanza, _ := raw.(map[string]any)
	addressing := cloneStringMap(stanza)
	for _, key := range []string{"match", "set-name", "macaddress", "interfaces", "openvswitch"} {
		delete(addressing, key)
	}
	return addressing
}

func (c openVSwitchNetplanCandidate) String() string {
	return filepath.Base(c.path) + " (" + c.uplinkName + ")"
}

func parseOpenVSwitchNetplanDocument(data []byte) (openVSwitchNetplanDocument, error) {
	root := map[string]any{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return openVSwitchNetplanDocument{}, fmt.Errorf("parse YAML: %w", err)
	}

	rawNetwork, exists := root["network"]
	if !exists {
		return openVSwitchNetplanDocument{}, errors.New("netplan network section is required")
	}
	network, ok := rawNetwork.(map[string]any)
	if !ok {
		return openVSwitchNetplanDocument{}, errors.New("netplan network section must be a mapping")
	}
	ethernets, err := openVSwitchNetplanSectionOrCreate(network, "ethernets")
	if err != nil {
		return openVSwitchNetplanDocument{}, err
	}
	bridges, err := openVSwitchNetplanSectionOrCreate(network, "bridges")
	if err != nil {
		return openVSwitchNetplanDocument{}, err
	}
	return openVSwitchNetplanDocument{
		root:      root,
		ethernets: ethernets,
		bridges:   bridges,
	}, nil
}

// openVSwitchNetplanSectionOrCreate creates an absent optional section, but
// rejects a present non-mapping value instead of silently replacing user data.
func openVSwitchNetplanSectionOrCreate(network map[string]any, name string) (map[string]any, error) {
	rawSection, exists := network[name]
	if !exists {
		section := map[string]any{}
		network[name] = section
		return section, nil
	}
	section, ok := rawSection.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("netplan network.%s section must be a mapping", name)
	}
	return section, nil
}

func (s *NodeAgentServer) buildOpenVSwitchNetplanPlan(link openVSwitchLink, gateway, bridgeName string) (openVSwitchPlan, error) {
	paths, err := filepath.Glob(filepath.Join(s.rootedPath(openVSwitchNetplanDir), "*.yaml"))
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("list netplan files: %w", err)
	}
	if len(paths) == 0 {
		return openVSwitchPlan{}, errors.New("neither ifupdown nor netplan configuration is present")
	}

	var candidates []openVSwitchNetplanCandidate
	var allENCandidates []openVSwitchNetplanCandidate
	documents := map[string]openVSwitchNetplanDocument{}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		document, parseErr := parseOpenVSwitchNetplanDocument(data)
		if parseErr != nil {
			return openVSwitchPlan{}, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		documents[path] = document
		// Netplan may have been written before the connection dropped. Treat
		// that bridge stanza as pending desired state and retry activation.
		if openVSwitchBridgeUsesInterface(document.bridges[bridgeName], link.Name) {
			candidates = append(candidates, openVSwitchNetplanCandidate{
				path:       path,
				document:   document,
				uplinkName: link.Name,
				pending:    true,
			})
			continue
		}
		for name, raw := range document.ethernets {
			stanza, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			candidate := openVSwitchNetplanCandidate{path: path, document: document, uplinkName: name}
			if name == link.Name || openVSwitchStanzaMatchesLink(stanza, link) {
				candidates = append(candidates, candidate)
			}
			if name == "all-en" {
				allENCandidates = append(allENCandidates, candidate)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = allENCandidates
	}
	if len(candidates) == 0 {
		return openVSwitchPlan{}, fmt.Errorf("expected one netplan configuration for %s, got 0", link.Name)
	}

	slices.SortStableFunc(candidates, func(a, b openVSwitchNetplanCandidate) int {
		return cmp.Compare(b.specificity(link), a.specificity(link))
	})
	selected := candidates[0]
	selectedAddressing := selected.addressing(bridgeName)
	duplicatePaths := map[string]bool{}
	for _, duplicate := range candidates[1:] {
		if !reflect.DeepEqual(duplicate.addressing(bridgeName), selectedAddressing) {
			return openVSwitchPlan{}, fmt.Errorf("netplan configures %s differently in %s and %s; leave one entry for it",
				link.Name, selected, duplicate)
		}
		delete(duplicate.document.ethernets, duplicate.uplinkName)
		if duplicate.path != selected.path {
			duplicatePaths[duplicate.path] = true
		}
		slog.Info("merging a duplicate netplan entry for the Open vSwitch uplink",
			"interface", link.Name, "kept", selected.String(), "removed", duplicate.String())
	}
	var duplicates []openVSwitchNetplanFile
	for _, path := range slices.Sorted(maps.Keys(duplicatePaths)) {
		data, err := renderOpenVSwitchNetplanDocument(documents[path])
		if err != nil {
			return openVSwitchPlan{}, fmt.Errorf("render %s: %w", path, err)
		}
		duplicates = append(duplicates, openVSwitchNetplanFile{path: path, data: data})
	}
	if !selected.pending {
		rawStanza, ok := selected.document.ethernets[selected.uplinkName]
		if !ok {
			return openVSwitchPlan{}, fmt.Errorf("netplan interface %s disappeared during reconciliation", selected.uplinkName)
		}
		bridgeStanza, ok := rawStanza.(map[string]any)
		if !ok {
			return openVSwitchPlan{}, fmt.Errorf("netplan interface %s is not an object", selected.uplinkName)
		}
		// Move the complete L3 stanza to the bridge so DHCP, static addresses,
		// routes, and DNS survive. The physical NIC keeps only its L2 identity.
		bridgeStanza = cloneStringMap(bridgeStanza)
		delete(bridgeStanza, "match")
		delete(bridgeStanza, "set-name")
		delete(bridgeStanza, "macaddress")
		bridgeStanza["interfaces"] = []string{link.Name}
		bridgeStanza["openvswitch"] = map[string]any{}
		selected.document.bridges[bridgeName] = bridgeStanza
		delete(selected.document.ethernets, selected.uplinkName)
		physical := map[string]any{"mtu": link.MTU}
		if link.Address != "" {
			physical["match"] = map[string]any{"macaddress": link.Address}
		}
		selected.document.ethernets[link.Name] = physical
	}
	data, err := yaml.Marshal(selected.document.root)
	if err != nil {
		return openVSwitchPlan{}, fmt.Errorf("render %s: %w", selected.path, err)
	}
	return openVSwitchPlan{
		backend:           pb.OpenVSwitchReconciliationResult_NETWORK_BACKEND_NETPLAN,
		bridgeName:        bridgeName,
		uplinkName:        link.Name,
		uplinkMAC:         link.Address,
		uplinkMTU:         link.MTU,
		defaultGateway:    gateway,
		configPath:        selected.path,
		config:            data,
		netplanDuplicates: duplicates,
	}, nil
}

// renderOpenVSwitchNetplanDocument drops the sections the parser created or a
// merge emptied, so a file left with no entries is just its version.
func renderOpenVSwitchNetplanDocument(document openVSwitchNetplanDocument) ([]byte, error) {
	network, _ := document.root["network"].(map[string]any)
	for _, name := range []string{"ethernets", "bridges"} {
		if section, ok := network[name].(map[string]any); ok && len(section) == 0 {
			delete(network, name)
		}
	}
	return yaml.Marshal(document.root)
}

func (s *NodeAgentServer) applyOpenVSwitchNetplanPlan(ctx context.Context, plan openVSwitchPlan) error {
	// Duplicates go first: a retry after a partial write then still finds the
	// bridged entry, or the original one, but never two that disagree.
	for _, duplicate := range plan.netplanDuplicates {
		if err := atomicWriteFile(duplicate.path, duplicate.data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", duplicate.path, err)
		}
	}
	if err := atomicWriteFile(plan.configPath, plan.config, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.configPath, err)
	}
	if _, err := s.runOpenVSwitchCommand("netplan", "generate"); err != nil {
		return fmt.Errorf("validate netplan: %w", err)
	}
	// OVS auto-inherits globally unique NIC MACs, but not locally administered
	// ones, such as a KubeVirt pod-network NIC's. Pin the latter before netplan
	// moves DHCP to the bridge, so the DHCP server still knows the client.
	if openVSwitchMACIsLocallyAdministered(plan.uplinkMAC) {
		if _, err := s.runOpenVSwitchCommand("ovs-vsctl", "--may-exist", "add-br", plan.bridgeName); err != nil {
			return err
		}
		if _, err := s.runOpenVSwitchCommand("ovs-vsctl", "set", "bridge", plan.bridgeName, "other-config:hwaddr="+plan.uplinkMAC); err != nil {
			return err
		}
	}
	if _, err := s.runOpenVSwitchCommand("netplan", "apply"); err != nil {
		return fmt.Errorf("apply netplan: %w", err)
	}
	return sleepOrDone(ctx, s.openVSwitchSettleDelayOrDefault())
}

func cloneStringMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func openVSwitchStanzaMatchesLink(stanza map[string]any, link openVSwitchLink) bool {
	match, _ := stanza["match"].(map[string]any)
	if name, _ := match["name"].(string); name != "" {
		matched, err := filepath.Match(name, link.Name)
		if err == nil && matched {
			return true
		}
	}
	mac, _ := match["macaddress"].(string)
	return link.Address != "" && strings.EqualFold(mac, link.Address)
}

func openVSwitchBridgeUsesInterface(raw any, interfaceName string) bool {
	stanza, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	interfaces, ok := stanza["interfaces"].([]any)
	if !ok {
		if typed, typedOK := stanza["interfaces"].([]string); typedOK {
			return slices.Contains(typed, interfaceName)
		}
		return false
	}
	for _, current := range interfaces {
		if current == interfaceName {
			return true
		}
	}
	return false
}
