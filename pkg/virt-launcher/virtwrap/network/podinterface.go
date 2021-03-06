/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/log"
	"kubevirt.io/client-go/precond"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var bridgeFakeIP = "169.254.75.1%d/32"

type BindMechanism interface {
	discoverPodNetworkInterface() error
	preparePodNetworkInterfaces() error

	loadCachedInterface(pid, name string) (bool, error)
	setCachedInterface(pid, name string) error

	// virt-handler that executes phase1 of network configuration needs to
	// pass details about discovered networking port into phase2 that is
	// executed by virt-launcher. Virt-launcher cannot discover some of
	// these details itself because at this point phase1 is complete and
	// ports are rewired, meaning, routes and IP addresses configured by
	// CNI plugin may be gone. For this matter, we use a cached VIF file to
	// pass discovered information between phases.
	loadCachedVIF(pid, name string) (bool, error)
	setCachedVIF(pid, name string) error

	// The following entry points require domain initialized for the
	// binding and can be used in phase2 only.
	decorateConfig() error
	startDHCP(vmi *v1.VirtualMachineInstance) error
}

type PodInterface struct{}

func (l *PodInterface) Unplug() {}

func getVifFilePath(pid, name string) string {
	return fmt.Sprintf(vifCacheFile, pid, name)
}

func writeVifFile(buf []byte, pid, name string) error {
	err := ioutil.WriteFile(getVifFilePath(pid, name), buf, 0644)
	if err != nil {
		return fmt.Errorf("error writing vif object: %v", err)
	}
	return nil
}

func (l *PodInterface) PlugPhase1(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string, pid int) error {
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	driver, err := getPhase1Binding(vmi, iface, network, podInterfaceName)
	if err != nil {
		return err
	}

	pidStr := fmt.Sprintf("%d", pid)
	isExist, err := driver.loadCachedInterface(pidStr, iface.Name)
	if err != nil {
		return err
	}

	if !isExist {
		err := driver.discoverPodNetworkInterface()
		if err != nil {
			return err
		}

		if err := driver.preparePodNetworkInterfaces(); err != nil {
			log.Log.Reason(err).Critical("failed to prepare pod networking")
			panic(err)
		}

		err = driver.setCachedInterface(pidStr, iface.Name)
		if err != nil {
			log.Log.Reason(err).Critical("failed to save interface configuration")
			panic(err)
		}

		err = driver.setCachedVIF(pidStr, iface.Name)
		if err != nil {
			log.Log.Reason(err).Critical("failed to save vif configuration")
			panic(err)
		}
	}

	return nil
}

func ensureDHCP(vmi *v1.VirtualMachineInstance, driver BindMechanism, podInterfaceName string) error {
	dhcpStartedFile := fmt.Sprintf("/var/run/kubevirt-private/dhcp_started-%s", podInterfaceName)
	_, err := os.Stat(dhcpStartedFile)
	if os.IsNotExist(err) {
		if err := driver.startDHCP(vmi); err != nil {
			return fmt.Errorf("failed to start DHCP server for interface %s", podInterfaceName)
		}
		newFile, err := os.Create(dhcpStartedFile)
		if err != nil {
			return fmt.Errorf("failed to create dhcp started file %s: %s", dhcpStartedFile, err)
		}
		newFile.Close()
	}
	return nil
}

func (l *PodInterface) PlugPhase2(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) error {
	precond.MustNotBeNil(domain)
	initHandler()

	// There is nothing to plug for SR-IOV devices
	if iface.SRIOV != nil {
		return nil
	}

	driver, err := getPhase2Binding(vmi, iface, network, domain, podInterfaceName)
	if err != nil {
		return err
	}

	pid := "self"

	isExist, err := driver.loadCachedInterface(pid, iface.Name)
	if err != nil {
		log.Log.Reason(err).Critical("failed to load cached interface configuration")
		panic(err)
	}
	if !isExist {
		log.Log.Reason(err).Critical("cached interface configuration doesn't exist")
		panic(errors.New("cached interface configuration doesn't exist"))
	}

	isExist, err = driver.loadCachedVIF(pid, iface.Name)
	if err != nil {
		log.Log.Reason(err).Critical("failed to load cached vif configuration")
		panic(err)
	}
	if !isExist {
		log.Log.Reason(err).Critical("cached vif configuration doesn't exist")
		panic(errors.New("cached vif configuration doesn't exist"))
	}

	err = driver.decorateConfig()
	if err != nil {
		log.Log.Reason(err).Critical("failed to create libvirt configuration")
		panic(err)
	}

	err = ensureDHCP(vmi, driver, podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Criticalf("failed to ensure dhcp service running for %s: %s", podInterfaceName, err)
		panic(err)
	}

	return nil
}

// The only difference between bindings for two phases is that the first phase
// should not require access to domain definition, hence we pass nil instead of
// it. This means that any functions called under phase1 code path should not
// use the domain set on the binding.
func getPhase1Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, podInterfaceName string) (BindMechanism, error) {
	return getPhase2Binding(vmi, iface, network, nil, podInterfaceName)
}

func getPhase2Binding(vmi *v1.VirtualMachineInstance, iface *v1.Interface, network *v1.Network, domain *api.Domain, podInterfaceName string) (BindMechanism, error) {
	populateMacAddress := func(vif *VIF, iface *v1.Interface) error {
		if iface.MacAddress != "" {
			macAddress, err := net.ParseMAC(iface.MacAddress)
			if err != nil {
				return err
			}
			vif.MAC = macAddress
		}
		return nil
	}

	if iface.Bridge != nil {
		vif := &VIF{Name: podInterfaceName}
		populateMacAddress(vif, iface)
		return &BridgePodInterface{iface: iface,
			virtIface:           &api.Interface{},
			vmi:                 vmi,
			vif:                 vif,
			domain:              domain,
			podInterfaceName:    podInterfaceName,
			bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
	}
	if iface.Masquerade != nil {
		vif := &VIF{Name: podInterfaceName}
		populateMacAddress(vif, iface)
		return &MasqueradePodInterface{iface: iface,
			virtIface:           &api.Interface{},
			vmi:                 vmi,
			vif:                 vif,
			domain:              domain,
			podInterfaceName:    podInterfaceName,
			vmNetworkCIDR:       network.Pod.VMNetworkCIDR,
			bridgeInterfaceName: fmt.Sprintf("k6t-%s", podInterfaceName)}, nil
	}
	if iface.Slirp != nil {
		return &SlirpPodInterface{vmi: vmi, iface: iface, domain: domain}, nil
	}
	return nil, fmt.Errorf("Not implemented")
}

type BridgePodInterface struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
}

func (b *BridgePodInterface) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(b.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", b.podInterfaceName)
		return err
	}
	b.podNicLink = link

	// get IP address
	addrList, err := Handler.AddrList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get an ip address for %s", b.podInterfaceName)
		return err
	}
	if len(addrList) == 0 {
		b.vif.IPAMDisabled = true
	} else {
		b.vif.IP = addrList[0]
		b.vif.IPAMDisabled = false
	}

	if len(b.vif.MAC) == 0 {
		// Get interface MAC address
		mac, err := Handler.GetMacDetails(b.podInterfaceName)
		if err != nil {
			log.Log.Reason(err).Errorf("failed to get MAC for %s", b.podInterfaceName)
			return err
		}
		b.vif.MAC = mac
	}

	if b.podNicLink.Attrs().MTU < 0 || b.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	b.vif.Mtu = uint16(b.podNicLink.Attrs().MTU)

	if !b.vif.IPAMDisabled {
		// Handle interface routes
		if err := b.setInterfaceRoutes(); err != nil {
			return err
		}
	}
	return nil
}

func (b *BridgePodInterface) getFakeBridgeIP() (string, error) {
	ifaces := b.vmi.Spec.Domain.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Name == b.iface.Name {
			return fmt.Sprintf(bridgeFakeIP, i), nil
		}
	}
	return "", fmt.Errorf("Failed to generate bridge fake address for interface %s", b.iface.Name)
}

func (b *BridgePodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	if !b.vif.IPAMDisabled {
		addr, err := b.getFakeBridgeIP()
		if err != nil {
			return err
		}
		fakeServerAddr, err := netlink.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address while starting DHCP server: %s", addr)
		}
		log.Log.Object(b.vmi).Infof("bridge pod interface: %+v %+v", b.vif, b)
		return Handler.StartDHCP(b.vif, fakeServerAddr, b.bridgeInterfaceName, b.iface.DHCPOptions)
	}
	return nil
}

func (b *BridgePodInterface) preparePodNetworkInterfaces() error {
	// Set interface link to down to change its MAC address
	if err := Handler.LinkSetDown(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link down for interface: %s", b.podInterfaceName)
		return err
	}

	if _, err := Handler.SetRandomMac(b.podInterfaceName); err != nil {
		return err
	}

	if err := Handler.LinkSetUp(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.podInterfaceName)
		return err
	}

	if err := b.createBridge(); err != nil {
		return err
	}

	if !b.vif.IPAMDisabled {
		// Remove IP from POD interface
		err := Handler.AddrDel(b.podNicLink, &b.vif.IP)

		if err != nil {
			log.Log.Reason(err).Errorf("failed to delete address for interface: %s", b.podInterfaceName)
			return err
		}
	}

	if err := Handler.LinkSetLearningOff(b.podNicLink); err != nil {
		log.Log.Reason(err).Errorf("failed to disable mac learning for interface: %s", b.podInterfaceName)
		return err
	}

	b.virtIface.MTU = &api.MTU{Size: strconv.Itoa(b.podNicLink.Attrs().MTU)}
	b.virtIface.MAC = &api.MAC{MAC: b.vif.MAC.String()}

	return nil
}

func (b *BridgePodInterface) decorateConfig() error {
	ifaces := b.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == b.iface.Name {
			ifaces[i].MTU = b.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: b.vif.MAC.String()}
			break
		}
	}
	return nil
}

func (b *BridgePodInterface) loadCachedInterface(pid, name string) (bool, error) {
	var ifaceConfig api.Interface

	isExist, err := readFromCachedFile(pid, name, interfaceCacheFile, &ifaceConfig)
	if err != nil {
		return false, err
	}

	if isExist {
		b.virtIface = &ifaceConfig
		return true, nil
	}

	return false, nil
}

func (b *BridgePodInterface) setCachedInterface(pid, name string) error {
	err := writeToCachedFile(b.virtIface, interfaceCacheFile, pid, name)
	return err
}

func (b *BridgePodInterface) loadCachedVIF(pid, name string) (bool, error) {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, name))
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(buf, &b.vif)
	if err != nil {
		return false, err
	}
	b.vif.Gateway = b.vif.Gateway.To4()
	return true, nil
}

func (b *BridgePodInterface) setCachedVIF(pid, name string) error {
	buf, err := json.MarshalIndent(&b.vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}
	return writeVifFile(buf, pid, name)
}

func (b *BridgePodInterface) setInterfaceRoutes() error {
	routes, err := Handler.RouteList(b.podNicLink, netlink.FAMILY_V4)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get routes for %s", b.podInterfaceName)
		return err
	}
	if len(routes) == 0 {
		return fmt.Errorf("No gateway address found in routes for %s", b.podInterfaceName)
	}
	b.vif.Gateway = routes[0].Gw
	if len(routes) > 1 {
		dhcpRoutes := filterPodNetworkRoutes(routes, b.vif)
		b.vif.Routes = &dhcpRoutes
	}
	return nil
}

func (b *BridgePodInterface) createBridge() error {
	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: b.bridgeInterfaceName,
		},
	}
	err := Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(b.podNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect interface %s to bridge %s", b.podInterfaceName, bridge.Name)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	// set fake ip on a bridge
	addr, err := b.getFakeBridgeIP()
	if err != nil {
		return err
	}
	fakeaddr, err := Handler.ParseAddr(addr)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", b.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, fakeaddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	return nil
}

type MasqueradePodInterface struct {
	vmi                 *v1.VirtualMachineInstance
	vif                 *VIF
	iface               *v1.Interface
	virtIface           *api.Interface
	podNicLink          netlink.Link
	domain              *api.Domain
	podInterfaceName    string
	bridgeInterfaceName string
	vmNetworkCIDR       string
	gatewayAddr         *netlink.Addr
}

func (p *MasqueradePodInterface) discoverPodNetworkInterface() error {
	link, err := Handler.LinkByName(p.podInterfaceName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to get a link for interface: %s", p.podInterfaceName)
		return err
	}
	p.podNicLink = link

	if p.podNicLink.Attrs().MTU < 0 || p.podNicLink.Attrs().MTU > 65535 {
		return fmt.Errorf("MTU value out of range ")
	}

	// Get interface MTU
	p.vif.Mtu = uint16(p.podNicLink.Attrs().MTU)

	if p.vmNetworkCIDR == "" {
		p.vmNetworkCIDR = api.DefaultVMCIDR
	}

	defaultGateway, vm, err := Handler.GetHostAndGwAddressesFromCIDR(p.vmNetworkCIDR)
	if err != nil {
		log.Log.Errorf("failed to get gw and vm available addresses from CIDR %s", p.vmNetworkCIDR)
		return err
	}

	gatewayAddr, err := Handler.ParseAddr(defaultGateway)
	if err != nil {
		return fmt.Errorf("failed to parse gateway ip address %s", defaultGateway)
	}
	p.vif.Gateway = gatewayAddr.IP.To4()
	p.gatewayAddr = gatewayAddr

	vmAddr, err := Handler.ParseAddr(vm)
	if err != nil {
		return fmt.Errorf("failed to parse vm ip address %s", vm)
	}
	p.vif.IP = *vmAddr

	return nil
}

func (p *MasqueradePodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	addr := fmt.Sprintf("%s/24", p.vif.Gateway.String())
	fakeServerAddr, err := netlink.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("failed to parse address while starting DHCP server: %s", addr)
	}
	return Handler.StartDHCP(p.vif, fakeServerAddr, p.bridgeInterfaceName, p.iface.DHCPOptions)
}

func (p *MasqueradePodInterface) preparePodNetworkInterfaces() error {
	// Create an master bridge interface
	bridgeNicName := fmt.Sprintf("%s-nic", p.bridgeInterfaceName)
	bridgeNic := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeNicName,
		},
	}
	err := Handler.LinkAdd(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create an interface: %s", bridgeNic.Name)
		return err
	}

	if p.iface.MacAddress == "" {
		p.vif.MAC, err = Handler.GenerateRandomMac()
		if err != nil {
			log.Log.Reason(err).Errorf("failed to generate random mac address")
			return err
		}
	}

	err = Handler.LinkSetUp(bridgeNic)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", bridgeNic.Name)
		return err
	}

	if err := p.createBridge(); err != nil {
		return err
	}

	err = p.createNatRules()
	if err != nil {
		log.Log.Errorf("failed to create nat rules for vm error: %v", err)
		return err
	}

	p.virtIface.MTU = &api.MTU{Size: strconv.Itoa(p.podNicLink.Attrs().MTU)}
	p.virtIface.MAC = &api.MAC{MAC: p.vif.MAC.String()}

	return nil
}

func (p *MasqueradePodInterface) decorateConfig() error {
	ifaces := p.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == p.iface.Name {
			ifaces[i].MTU = p.virtIface.MTU
			ifaces[i].MAC = &api.MAC{MAC: p.vif.MAC.String()}
			break
		}
	}
	return nil
}

func (p *MasqueradePodInterface) loadCachedInterface(pid, name string) (bool, error) {
	var ifaceConfig api.Interface

	isExist, err := readFromCachedFile(pid, name, interfaceCacheFile, &ifaceConfig)
	if err != nil {
		return false, err
	}

	if isExist {
		p.virtIface = &ifaceConfig
		return true, nil
	}

	return false, nil
}

func (p *MasqueradePodInterface) setCachedInterface(pid, name string) error {
	err := writeToCachedFile(p.virtIface, interfaceCacheFile, pid, name)
	return err
}

func (p *MasqueradePodInterface) loadCachedVIF(pid, name string) (bool, error) {
	buf, err := ioutil.ReadFile(getVifFilePath(pid, name))
	if err != nil {
		return false, err
	}
	err = json.Unmarshal(buf, &p.vif)
	if err != nil {
		return false, err
	}
	p.vif.Gateway = p.vif.Gateway.To4()
	return true, nil
}

func (p *MasqueradePodInterface) setCachedVIF(pid, name string) error {
	buf, err := json.MarshalIndent(&p.vif, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling vif object: %v", err)
	}
	return writeVifFile(buf, pid, name)
}

func (p *MasqueradePodInterface) createBridge() error {
	// Get dummy link
	bridgeNicName := fmt.Sprintf("%s-nic", p.bridgeInterfaceName)
	bridgeNicLink, err := Handler.LinkByName(bridgeNicName)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to find dummy interface for bridge")
		return err
	}

	// Create a bridge
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: p.bridgeInterfaceName,
		},
	}
	err = Handler.LinkAdd(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to create a bridge")
		return err
	}

	err = Handler.LinkSetMaster(bridgeNicLink, bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to connect %s interface to bridge %s", bridgeNicName, p.bridgeInterfaceName)
		return err
	}

	err = Handler.LinkSetUp(bridge)
	if err != nil {
		log.Log.Reason(err).Errorf("failed to bring link up for interface: %s", p.bridgeInterfaceName)
		return err
	}

	if err := Handler.AddrAdd(bridge, p.gatewayAddr); err != nil {
		log.Log.Reason(err).Errorf("failed to set bridge IP")
		return err
	}

	return nil
}

func (p *MasqueradePodInterface) createNatRules() error {
	if Handler.UseIptables() {
		return p.createNatRulesUsingIptables()
	}
	return p.createNatRulesUsingNftables()
}

func (p *MasqueradePodInterface) createNatRulesUsingIptables() error {
	err := Handler.IptablesNewChain("nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesNewChain("nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule("nat", "POSTROUTING", "-s", p.vif.IP.IP.String(), "-j", "MASQUERADE")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule("nat", "PREROUTING", "-i", p.podInterfaceName, "-j", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.IptablesAppendRule("nat", "POSTROUTING", "-o", p.bridgeInterfaceName, "-j", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(p.iface.Ports) == 0 {
		err = Handler.IptablesAppendRule("nat", "KUBEVIRT_PREINBOUND",
			"-j",
			"DNAT",
			"--to-destination", p.vif.IP.IP.String())

		return err
	}

	for _, port := range p.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.IptablesAppendRule("nat", "KUBEVIRT_POSTINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"-j",
			"SNAT",
			"--to-source", p.gatewayAddr.IP.String())
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule("nat", "KUBEVIRT_PREINBOUND",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"-j",
			"DNAT",
			"--to-destination", p.vif.IP.IP.String())
		if err != nil {
			return err
		}

		err = Handler.IptablesAppendRule("nat", "OUTPUT",
			"-p",
			strings.ToLower(port.Protocol),
			"--dport",
			strconv.Itoa(int(port.Port)),
			"--destination", "127.0.0.1",
			"-j",
			"DNAT",
			"--to-destination", p.vif.IP.IP.String())
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *MasqueradePodInterface) createNatRulesUsingNftables() error {
	err := Handler.NftablesLoad("ipv4-nat")
	if err != nil {
		return err
	}

	err = Handler.NftablesNewChain("nat", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesNewChain("nat", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule("nat", "postrouting", "ip", "saddr", p.vif.IP.IP.String(), "counter", "masquerade")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule("nat", "prerouting", "iifname", p.podInterfaceName, "counter", "jump", "KUBEVIRT_PREINBOUND")
	if err != nil {
		return err
	}

	err = Handler.NftablesAppendRule("nat", "postrouting", "oifname", p.bridgeInterfaceName, "counter", "jump", "KUBEVIRT_POSTINBOUND")
	if err != nil {
		return err
	}

	if len(p.iface.Ports) == 0 {
		err = Handler.NftablesAppendRule("nat", "KUBEVIRT_PREINBOUND",
			"counter", "dnat", "to", p.vif.IP.IP.String())

		return err
	}

	for _, port := range p.iface.Ports {
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}

		err = Handler.NftablesAppendRule("nat", "KUBEVIRT_POSTINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "snat", "to", p.gatewayAddr.IP.String())
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule("nat", "KUBEVIRT_PREINBOUND",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", p.vif.IP.IP.String())
		if err != nil {
			return err
		}

		err = Handler.NftablesAppendRule("nat", "output",
			"ip", "daddr", "127.0.0.1",
			strings.ToLower(port.Protocol),
			"dport",
			strconv.Itoa(int(port.Port)),
			"counter", "dnat", "to", p.vif.IP.IP.String())
		if err != nil {
			return err
		}
	}

	return nil
}

type SlirpPodInterface struct {
	vmi       *v1.VirtualMachineInstance
	iface     *v1.Interface
	virtIface *api.Interface
	domain    *api.Domain
}

func (s *SlirpPodInterface) discoverPodNetworkInterface() error {
	return nil
}

func (s *SlirpPodInterface) preparePodNetworkInterfaces() error {
	return nil
}

func (s *SlirpPodInterface) startDHCP(vmi *v1.VirtualMachineInstance) error {
	return nil
}

func (s *SlirpPodInterface) decorateConfig() error {
	// remove slirp interface from domain spec devices interfaces
	var foundIface *api.Interface
	ifaces := s.domain.Spec.Devices.Interfaces
	for i, iface := range ifaces {
		if iface.Alias.Name == s.iface.Name {
			s.domain.Spec.Devices.Interfaces = append(ifaces[:i], ifaces[i+1:]...)
			foundIface = &iface
			break
		}
	}

	if foundIface == nil {
		return fmt.Errorf("failed to find interface %s in vmi spec", s.iface.Name)
	}

	qemuArg := fmt.Sprintf("%s,netdev=%s,id=%s", foundIface.Model.Type, s.iface.Name, s.iface.Name)
	if s.iface.MacAddress != "" {
		// We assume address was already validated in API layer so just pass it to libvirt as-is.
		qemuArg += fmt.Sprintf(",mac=%s", s.iface.MacAddress)
	}
	// Add interface configuration to qemuArgs
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: "-device"})
	s.domain.Spec.QEMUCmd.QEMUArg = append(s.domain.Spec.QEMUCmd.QEMUArg, api.Arg{Value: qemuArg})

	return nil
}

func (s *SlirpPodInterface) loadCachedInterface(pid, name string) (bool, error) {
	return true, nil
}

func (s *SlirpPodInterface) loadCachedVIF(pid, name string) (bool, error) {
	return true, nil
}

func (b *SlirpPodInterface) setCachedVIF(pid, name string) error {
	return nil
}

func (s *SlirpPodInterface) setCachedInterface(pid, name string) error {
	return nil
}
