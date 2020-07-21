// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sriov

import (
	"context"
	"fmt"
	"github.com/networkservicemesh/sdk-sriov/pkg/sriov/utils"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"sync"

	"github.com/pkg/errors"
)

// VirtualFunctionState is a virtual function state
type VirtualFunctionState string

const (
	// UsedVirtualFunction is virtual function is use state
	UsedVirtualFunction VirtualFunctionState = "used"
	// FreeVirtualFunction is virtual function free state
	FreeVirtualFunction VirtualFunctionState = "free"
)

// NetResourcePool provides contains information about net devices
type NetResourcePool struct {
	HostName  string
	Resources []*NetResource
	lock      sync.Mutex
}

// SelectVirtualFunction marks one of the free virtual functions for specified physical function as in-use and returns it
func (n *NetResourcePool) SelectVirtualFunction(pfPCIAddr string) (selectedVf *VirtualFunction, err error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	for _, netResource := range n.Resources {
		pf := netResource.PhysicalFunction
		if pf.PCIAddress != pfPCIAddr {
			continue
		}

		// select the first free virtual function
		for vf, state := range pf.VirtualFunctions {
			if state == FreeVirtualFunction {
				selectedVf = vf
				break
			}
		}
		if selectedVf == nil {
			return nil, errors.Errorf("no free virtual function found for device %s", pfPCIAddr)
		}

		// mark it as in use
		err = pf.SetVirtualFunctionState(selectedVf, UsedVirtualFunction)
		if err != nil {
			return nil, err
		}

		return selectedVf, nil
	}

	return nil, errors.Errorf("no physical function with PCI address %s found", pfPCIAddr)
}

// ReleaseVirtualFunction marks given virtual function as free
func (n *NetResourcePool) ReleaseVirtualFunction(pfPCIAddr, vfNetIfaceName string) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	for _, netResource := range n.Resources {
		pf := netResource.PhysicalFunction
		if pf.PCIAddress != pfPCIAddr {
			continue
		}

		for vf := range pf.VirtualFunctions {
			if vf.NetInterfaceName == vfNetIfaceName {
				return pf.SetVirtualFunctionState(vf, FreeVirtualFunction)
			}
		}
		return errors.Errorf("no virtual function with net interface name %s found", vfNetIfaceName)
	}
	return errors.Errorf("no physical function with PCI address %s found", pfPCIAddr)
}

// GetFreeVirtualFunctionsInfo returns map containing number of free virtual functions for each physical function
// in the pool keyed by physical function's PCI address
func (n *NetResourcePool) GetFreeVirtualFunctionsInfo() *FreeVirtualFunctionsInfo {
	n.lock.Lock()
	defer n.lock.Unlock()

	info := &FreeVirtualFunctionsInfo{
		HostName:             n.HostName,
		FreeVirtualFunctions: map[string]int{},
	}

	for _, netResource := range n.Resources {
		pf := netResource.PhysicalFunction
		freeVfs := pf.GetFreeVirtualFunctionsNumber()
		info.FreeVirtualFunctions[pf.PCIAddress] = freeVfs
	}

	return info
}

func (n *NetResourcePool) AddNetDevices(config *ResourceDomain) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	sriovProvider := utils.NewSriovProvider(utils.SysfsDevicesPath)
	ctx := context.Background()

	for _, device := range config.PCIDevices {
		pfPciAddr := device.PCIAddress

		err := n.validateDevice(pfPciAddr)
		if err != nil {
			return fmt.Errorf("invalid device: %v", err)
		}

		exists, err := sriovProvider.IsDeviceExists(ctx, pfPciAddr)
		if err != nil {
			return err
		}
		if !exists {
			return errors.Errorf("Unable to find device: %s", pfPciAddr)
		}
		vfCapacity, err := sriovProvider.GetSriovVirtualFunctionsCapacity(ctx, pfPciAddr)
		if err != nil {
			return errors.Wrapf(err, "Unable to determine virtual functions capacity for device: %s", pfPciAddr)
		}

		physfun := &PhysicalFunction{
			PCIAddress:               pfPciAddr,
			VirtualFunctionsCapacity: vfCapacity,
			NetInterfaceName:         "",
			VirtualFunctions:         map[*VirtualFunction]VirtualFunctionState{},
		}

		pfIfaceNames, err := sriovProvider.GetNetInterfacesNames(ctx, pfPciAddr)
		if err != nil {
			return fmt.Errorf("unable to determine net interface name for device %s: %v", pfPciAddr, err)
		}
		physfun.NetInterfaceName = pfIfaceNames[0]

		err = sriovProvider.CreateVirtualFunctions(ctx, pfPciAddr, physfun.VirtualFunctionsCapacity)
		if err != nil {
			return fmt.Errorf("unable to create vitual functions for device %s: %v", pfPciAddr, err)
		}

		vfs, err := sriovProvider.GetVirtualFunctionsList(ctx, pfPciAddr)
		if err != nil {
			return fmt.Errorf("unable to discover vitual functions for device %s: %v", pfPciAddr, err)
		}

		for _, vfPciAddr := range vfs {
			vfIfaceNames, err := sriovProvider.GetNetInterfacesNames(ctx, vfPciAddr)
			if err != nil {
				return fmt.Errorf("unable to determine net interface name for device %s: %v", vfPciAddr, err)
			}
			vf := &VirtualFunction{
				PCIAddress:       vfPciAddr,
				NetInterfaceName: vfIfaceNames[0],
			}
			physfun.VirtualFunctions[vf] = FreeVirtualFunction
		}

		netRes := &NetResource{
			// TODO also check capability by checking device.GetLinkSpeed???
			Capability:       "",
			PhysicalFunction: physfun,
		}
		n.Resources = append(n.Resources, netRes)
	}
	return nil
}

func (rp *NetResourcePool) validateDevice(pciAddr string) error {
	sriovProvider := utils.NewSriovProvider(utils.SysfsDevicesPath)
	ctx := context.Background()

	if exists, err := sriovProvider.IsDeviceExists(ctx, pciAddr); err != nil || !exists {
		return err
	}

	if !sriovProvider.IsDeviceSriovCapable(ctx, pciAddr) {
		return fmt.Errorf("device %s is not SR-IOV capable", pciAddr)
	}

	// TODO think about what we do with already configured devices
	if sriovProvider.IsSriovConfigured(ctx, pciAddr) {
		return fmt.Errorf("device %s is alredy configured", pciAddr)
	}

	ifaceNames, err := sriovProvider.GetNetInterfacesNames(ctx, pciAddr)
	if err != nil {
		return fmt.Errorf("unable to determine net interface name for device %s: %v", pciAddr, err)
	}
	// exclude net device in-use in host
	if isDefaultRoute, _ := isDefaultRoute(ifaceNames); isDefaultRoute {
		return fmt.Errorf("device %s is in-use in host", pciAddr)
	}

	return nil
}

// NetResource contains information about net device
type NetResource struct {
	Capability       string
	PhysicalFunction *PhysicalFunction
}

// PhysicalFunction contains information about physical function
type PhysicalFunction struct {
	PCIAddress               string
	VirtualFunctionsCapacity int
	NetInterfaceName         string
	VirtualFunctions         map[*VirtualFunction]VirtualFunctionState
}

// SetVirtualFunctionState changes state of the given virtual function
func (p *PhysicalFunction) SetVirtualFunctionState(vf *VirtualFunction, state VirtualFunctionState) error {
	val, found := p.VirtualFunctions[vf]
	if !found {
		return errors.New("specified virtual function is not found")
	}
	if val == state {
		return errors.Errorf("specified virtual function is already %s", state)
	}
	p.VirtualFunctions[vf] = state
	return nil
}

// GetFreeVirtualFunctionsNumber returns number of virtual functions that have FreeVirtualFunction state
func (p *PhysicalFunction) GetFreeVirtualFunctionsNumber() int {
	freeVfs := 0
	for _, state := range p.VirtualFunctions {
		if state == FreeVirtualFunction {
			freeVfs++
		}
	}
	return freeVfs
}

// VirtualFunction contains information about virtual function
type VirtualFunction struct {
	PCIAddress       string
	NetInterfaceName string
}

// IsDefaultRoute returns true if PCI network device is default route interface
func isDefaultRoute(ifNames []string) (bool, error) {
	if len(ifNames) > 0 { // there's at least one interface name found
		for _, ifName := range ifNames {
			link, err := netlink.LinkByName(ifName)
			if err != nil {
				logrus.Errorf("expected to get valid host interface with name %s: %q", ifName, err)
				continue
			}

			routes, err := netlink.RouteList(link, netlink.FAMILY_V4) // IPv6 routes: all interface has at least one link local route entry
			if err != nil {
				logrus.Errorf("expected to get valid routes for interface with name %s: %q", ifName, err)
				continue
			}

			for idx := range routes {
				if routes[idx].Dst == nil {
					logrus.Infof("excluding interface %s: default route found: %+v", ifName, routes[idx])
					return true, nil
				}
			}
		}
	}
	return false, nil
}
