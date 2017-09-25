package ipam

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"strings"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/bitseq"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/discoverapi"
	"github.com/docker/libnetwork/ipamapi"
	"github.com/docker/libnetwork/ipamutils"
	"github.com/docker/libnetwork/types"
	"github.com/docker/libnetwork/ns"
	"github.com/vishvananda/netlink"
)

const (
	localAddressSpace  = "LocalDefault"
	globalAddressSpace = "GlobalDefault"
	// The biggest configurable host subnets
	minNetSize   = 8
	minNetSizeV6 = 64
	// datastore keyes for ipam objects
	dsConfigKey = "ipam/" + ipamapi.DefaultIPAM + "/config"
	dsDataKey   = "ipam/" + ipamapi.DefaultIPAM + "/data"
	dhcpInterface      = "dhcp_interface" // used for --ipam-opt dhcp_interface
)

// Allocator provides per address space ipv4/ipv6 book keeping
type Allocator struct {
	// Predefined pools for default address spaces
	predefined map[string][]*net.IPNet
	addrSpaces map[string]*addrSpace
	// stores        []datastore.Datastore
	// Allocated addresses in each address space's subnet
	addresses map[SubnetKey]*bitseq.Handle
	sync.Mutex
}

// NewAllocator returns an instance of libnetwork ipam
func NewAllocator(lcDs, glDs datastore.DataStore) (*Allocator, error) {
	a := &Allocator{}

	// Load predefined subnet pools
	a.predefined = map[string][]*net.IPNet{
		localAddressSpace:  ipamutils.PredefinedBroadNetworks,
		globalAddressSpace: ipamutils.PredefinedGranularNetworks,
	}

	// Initialize bitseq map
	a.addresses = make(map[SubnetKey]*bitseq.Handle)

	// Initialize address spaces
	a.addrSpaces = make(map[string]*addrSpace)
	for _, aspc := range []struct {
		as string
		ds datastore.DataStore
	}{
		{localAddressSpace, lcDs},
		{globalAddressSpace, glDs},
	} {
		a.initializeAddressSpace(aspc.as, aspc.ds)
	}

	return a, nil
}

func (a *Allocator) refresh(as string) error {
	aSpace, err := a.getAddressSpaceFromStore(as)
	if err != nil {
		return types.InternalErrorf("error getting pools config from store: %v", err)
	}

	if aSpace == nil {
		return nil
	}

	a.Lock()
	a.addrSpaces[as] = aSpace
	a.Unlock()

	return nil
}

func (a *Allocator) updateBitMasks(aSpace *addrSpace) error {
	var inserterList []func() error

	aSpace.Lock()
	for k, v := range aSpace.subnets {
		if v.Range == nil {
			kk := k
			vv := v
			inserterList = append(inserterList, func() error { return a.insertBitMask(kk, vv.Pool) })
		}
	}
	aSpace.Unlock()

	// Add the bitmasks (data could come from datastore)
	if inserterList != nil {
		for _, f := range inserterList {
			if err := f(); err != nil {
				return err
			}
		}
	}

	return nil
}

// Checks for and fixes damaged bitmask.
func (a *Allocator) checkConsistency(as string) {
	var sKeyList []SubnetKey

	// Retrieve this address space's configuration and bitmasks from the datastore
	a.refresh(as)
	a.Lock()
	aSpace, ok := a.addrSpaces[as]
	a.Unlock()
	if !ok {
		return
	}
	a.updateBitMasks(aSpace)

	aSpace.Lock()
	for sk, pd := range aSpace.subnets {
		if pd.Range != nil {
			continue
		}
		sKeyList = append(sKeyList, sk)
	}
	aSpace.Unlock()

	for _, sk := range sKeyList {
		a.Lock()
		bm := a.addresses[sk]
		a.Unlock()
		if err := bm.CheckConsistency(); err != nil {
			log.Warnf("Error while running consistency check for %s: %v", sk, err)
		}
	}
}

func (a *Allocator) initializeAddressSpace(as string, ds datastore.DataStore) error {
	scope := ""
	if ds != nil {
		scope = ds.Scope()
	}

	a.Lock()
	if currAS, ok := a.addrSpaces[as]; ok {
		if currAS.ds != nil {
			a.Unlock()
			return types.ForbiddenErrorf("a datastore is already configured for the address space %s", as)
		}
	}
	a.addrSpaces[as] = &addrSpace{
		subnets: map[SubnetKey]*PoolData{},
		id:      dsConfigKey + "/" + as,
		scope:   scope,
		ds:      ds,
		alloc:   a,
	}
	a.Unlock()

	a.checkConsistency(as)

	return nil
}

// DiscoverNew informs the allocator about a new global scope datastore
func (a *Allocator) DiscoverNew(dType discoverapi.DiscoveryType, data interface{}) error {
	if dType != discoverapi.DatastoreConfig {
		return nil
	}

	dsc, ok := data.(discoverapi.DatastoreConfigData)
	if !ok {
		return types.InternalErrorf("incorrect data in datastore update notification: %v", data)
	}

	ds, err := datastore.NewDataStoreFromConfig(dsc)
	if err != nil {
		return err
	}

	return a.initializeAddressSpace(globalAddressSpace, ds)
}

// DiscoverDelete is a notification of no interest for the allocator
func (a *Allocator) DiscoverDelete(dType discoverapi.DiscoveryType, data interface{}) error {
	return nil
}

// GetDefaultAddressSpaces returns the local and global default address spaces
func (a *Allocator) GetDefaultAddressSpaces() (string, string, error) {
	return localAddressSpace, globalAddressSpace, nil
}




// RequestPool returns an address pool along with its unique id.
func (a *Allocator) RequestPool(addressSpace, pool, subPool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
	log.Debugf("RequestPool(%s, %s, %s, %v, %t)", addressSpace, pool, subPool, options, v6)
retry:


	dp := &addrSpace{
		dhcpLeases: dhcpLeaseTable{},
	}

	for option, value := range options {
		switch option {
		case dhcpInterface:
			// parse DHCP interface option '--ipam-opt dhcp_interface=eth0'
			dp.DhcpInterface = value
			}
		}

	k, nw, ipr, pdf, err := a.parsePoolRequest(addressSpace, pool, subPool, v6)
	if err != nil {
		return "", nil, nil, types.InternalErrorf("failed to parse pool request for address space %q pool %q subpool %q: %v", addressSpace, pool, subPool, err)
	}

	if err := a.refresh(addressSpace); err != nil {
		return "", nil, nil, err
	}

	aSpace, err := a.getAddrSpace(addressSpace)
	if err != nil {
		return "", nil, nil, err
	}

	insert, err := aSpace.updatePoolDBOnAdd(*k, nw, ipr, pdf)
	if err != nil {
		if _, ok := err.(types.MaskableError); ok {
			log.Debugf("Retrying predefined pool search: %v", err)
			goto retry
		}
		return "", nil, nil, err
	}

	if err := a.writeToStore(aSpace); err != nil {
		if _, ok := err.(types.RetryError); !ok {
			return "", nil, nil, types.InternalErrorf("pool configuration failed because of %s", err.Error())
		}

		goto retry
	}

	return k.String(), nw, nil, insert()
}






// ReleasePool releases the address pool identified by the passed id
func (a *Allocator) ReleasePool(poolID string) error {
	log.Debugf("ReleasePool(%s)", poolID)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

retry:
	if err := a.refresh(k.AddressSpace); err != nil {
		return err
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return err
	}

	remove, err := aSpace.updatePoolDBOnRemoval(k)
	if err != nil {
		return err
	}

	if err = a.writeToStore(aSpace); err != nil {
		if _, ok := err.(types.RetryError); !ok {
			return types.InternalErrorf("pool (%s) removal failed because of %v", poolID, err)
		}
		goto retry
	}

	return remove()
}

// Given the address space, returns the local or global PoolConfig based on the
// address space is local or global. AddressSpace locality is being registered with IPAM out of band.
func (a *Allocator) getAddrSpace(as string) (*addrSpace, error) {
	a.Lock()
	defer a.Unlock()
	aSpace, ok := a.addrSpaces[as]
	if !ok {
		return nil, types.BadRequestErrorf("cannot find address space %s (most likely the backing datastore is not configured)", as)
	}
	return aSpace, nil
}

func (a *Allocator) parsePoolRequest(addressSpace, pool, subPool string, v6 bool) (*SubnetKey, *net.IPNet, *AddressRange, bool, error) {
	var (
		nw  *net.IPNet
		ipr *AddressRange
		err error
		pdf = false
	)

	if addressSpace == "" {
		return nil, nil, nil, false, ipamapi.ErrInvalidAddressSpace
	}

	if pool == "" && subPool != "" {
		return nil, nil, nil, false, ipamapi.ErrInvalidSubPool
	}

	if pool != "" {
		if _, nw, err = net.ParseCIDR(pool); err != nil {
			return nil, nil, nil, false, ipamapi.ErrInvalidPool
		}
		if subPool != "" {
			if ipr, err = getAddressRange(subPool, nw); err != nil {
				return nil, nil, nil, false, err
			}
		}
	} else {
		if nw, err = a.getPredefinedPool(addressSpace, v6); err != nil {
			return nil, nil, nil, false, err
		}
		pdf = true
	}

	return &SubnetKey{AddressSpace: addressSpace, Subnet: nw.String(), ChildSubnet: subPool}, nw, ipr, pdf, nil
}

func (a *Allocator) insertBitMask(key SubnetKey, pool *net.IPNet) error {
	//log.Debugf("Inserting bitmask (%s, %s)", key.String(), pool.String())

	store := a.getStore(key.AddressSpace)
	ipVer := getAddressVersion(pool.IP)
	ones, bits := pool.Mask.Size()
	numAddresses := uint64(1 << uint(bits-ones))

	// Allow /64 subnet
	if ipVer == v6 && numAddresses == 0 {
		numAddresses--
	}

	// Generate the new address masks. AddressMask content may come from datastore
	h, err := bitseq.NewHandle(dsDataKey, store, key.String(), numAddresses)
	if err != nil {
		return err
	}

	// Do not let network identifier address be reserved
	// Do the same for IPv6 so that bridge ip starts with XXXX...::1
	h.Set(0)

	// Do not let broadcast address be reserved
	if ipVer == v4 {
		h.Set(numAddresses - 1)
	}

	a.Lock()
	a.addresses[key] = h
	a.Unlock()
	return nil
}

func (a *Allocator) retrieveBitmask(k SubnetKey, n *net.IPNet) (*bitseq.Handle, error) {
	a.Lock()
	bm, ok := a.addresses[k]
	a.Unlock()
	if !ok {
		log.Debugf("Retrieving bitmask (%s, %s)", k.String(), n.String())
		if err := a.insertBitMask(k, n); err != nil {
			return nil, types.InternalErrorf("could not find bitmask in datastore for %s", k.String())
		}
		a.Lock()
		bm = a.addresses[k]
		a.Unlock()
	}
	return bm, nil
}

func (a *Allocator) getPredefineds(as string) []*net.IPNet {
	a.Lock()
	defer a.Unlock()
	l := make([]*net.IPNet, 0, len(a.predefined[as]))
	for _, pool := range a.predefined[as] {
		l = append(l, pool)
	}
	return l
}

func (a *Allocator) getPredefinedPool(as string, ipV6 bool) (*net.IPNet, error) {
	var v ipVersion
	v = v4
	if ipV6 {
		v = v6
	}

	if as != localAddressSpace && as != globalAddressSpace {
		return nil, types.NotImplementedErrorf("no default pool availbale for non-default addresss spaces")
	}

	aSpace, err := a.getAddrSpace(as)
	if err != nil {
		return nil, err
	}

	for _, nw := range a.getPredefineds(as) {
		if v != getAddressVersion(nw.IP) {
			continue
		}
		aSpace.Lock()
		_, ok := aSpace.subnets[SubnetKey{AddressSpace: as, Subnet: nw.String()}]
		aSpace.Unlock()
		if ok {
			continue
		}

		if !aSpace.contains(as, nw) {
			return nw, nil
		}
	}

	return nil, types.NotFoundErrorf("could not find an available, non-overlapping IPv%d address pool among the defaults to assign to the network", v)
}

// RequestAddress returns an address from the specified pool ID
func (a *Allocator) RequestAddress(poolID string, prefAddress net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	log.Debugf("RequestAddress(%s, %v, %v)", poolID, prefAddress, opts)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return nil, nil, types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

	if err := a.refresh(k.AddressSpace); err != nil {
		return nil, nil, err
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return nil, nil, err
	}

	aSpace.Lock()
	p, ok := aSpace.subnets[k]
	if !ok {
		aSpace.Unlock()
		return nil, nil, types.NotFoundErrorf("cannot find address pool for poolID:%s", poolID)
	}

	if prefAddress != nil && !p.Pool.Contains(prefAddress) {
		aSpace.Unlock()
		return nil, nil, ipamapi.ErrIPOutOfRange
	}

	c := p
	for c.Range != nil {
		k = c.ParentKey
		c, ok = aSpace.subnets[k]
	}
	aSpace.Unlock()

	bm, err := a.retrieveBitmask(k, c.Pool)
	if err != nil {
		return nil, nil, types.InternalErrorf("could not find bitmask in datastore for %s on address %v request from pool %s: %v",
			k.String(), prefAddress, poolID, err)
	}

	//macAddr := opts[netlabel.MacAddress]

	//if len(macAddr) <= 0 {
	//	return nil, nil, fmt.Errorf("no mac address found in the request address call")
	//}

	if len(opts["dhcp_interface"]) > 0 && len(opts["com.docker.network.endpoint.macaddress"]) > 0 {
			if !parentExists(opts["dhcp_interface"]) {
				// if the subinterface parent_iface.vlan_id checks do not pass, return err.
				//  a valid example is 'eth0.10' for a parent iface 'eth0' with a vlan id '10'
				err := createVlanLink(opts["dhcp_interface"])
				if err != nil {
					return nil, nil, fmt.Errorf("failed to create the %s subinterface: %v", opts["dhcp_interface"], err)
				}
			}

			ip, err := requestDHCPLease(opts["com.docker.network.endpoint.macaddress"], opts["dhcp_interface"])
			log.Debugf("lcb-20170220-0003:%v", ip)
			if err != nil {
				return nil, nil, err
				}
			return &net.IPNet{IP: ip, Mask: p.Pool.Mask}, nil, nil

	} else {
		ip, err := a.getAddress(p.Pool, bm, prefAddress, p.Range)
		log.Debugf("lcb-20170220-0004:%v", ip)
		if err != nil {
			return nil, nil, err
		}
		return &net.IPNet{IP: ip, Mask: p.Pool.Mask}, nil, nil
	}
}


func parentExists(ifaceStr string) bool {
	_, err := ns.NlHandle().LinkByName(ifaceStr)
	if err != nil {
		return false
	}

	return true
}

func parseVlan(linkName string) (string, int, error) {
	// parse -o parent=eth0.10
	splitName := strings.Split(linkName, ".")
	if len(splitName) != 2 {
		return "", 0, fmt.Errorf("required interface name format is: name.vlan_id, ex. eth0.10 for vlan 10, instead received %s", linkName)
	}
	parent, vidStr := splitName[0], splitName[1]
	// validate type and convert vlan id to int
	vidInt, err := strconv.Atoi(vidStr)
	if err != nil {
		return "", 0, fmt.Errorf("unable to parse a valid vlan id from: %s (ex. eth0.10 for vlan 10)", vidStr)
	}
	// Check if the interface exists
	if !parentExists(parent) {
		return "", 0, fmt.Errorf("-o parent interface does was not found on the host: %s", parent)
	}

	return parent, vidInt, nil
}

func createVlanLink(parentName string) error {
	if strings.Contains(parentName, ".") {
		parent, vidInt, err := parseVlan(parentName)
		if err != nil {
			return err
		}
		// VLAN identifier or VID is a 12-bit field specifying the VLAN to which the frame belongs
		if vidInt > 4094 || vidInt < 1 {
			return fmt.Errorf("vlan id must be between 1-4094, received: %d", vidInt)
		}
		// get the parent link to attach a vlan subinterface
		parentLink, err := ns.NlHandle().LinkByName(parent)
		if err != nil {
			return fmt.Errorf("failed to find master interface %s on the Docker host: %v", parent, err)
		}
		vlanLink := &netlink.Vlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        parentName,
				ParentIndex: parentLink.Attrs().Index,
			},
			VlanId: vidInt,
		}
		// create the subinterface
		if err := ns.NlHandle().LinkAdd(vlanLink); err != nil {
			return fmt.Errorf("failed to create %s vlan link: %v", vlanLink.Name, err)
		}
		// Bring the new netlink iface up
		if err := ns.NlHandle().LinkSetUp(vlanLink); err != nil {
			return fmt.Errorf("failed to enable %s the macvlan parent link %v", vlanLink.Name, err)
		}
		log.Debugf("Added a vlan tagged netlink subinterface: %s with a vlan id: %d", parentName, vidInt)
		return nil
	}

	return fmt.Errorf("invalid parent name %s, examples are \"eth0,eth1,eth0.10\"", parentName)
}


// ReleaseAddress releases the address from the specified pool ID
func (a *Allocator) ReleaseAddress(poolID string, address net.IP) error {
	log.Debugf("ReleaseAddress(%s, %v)", poolID, address)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

	if err := a.refresh(k.AddressSpace); err != nil {
		return err
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return err
	}

	aSpace.Lock()
	p, ok := aSpace.subnets[k]
	if !ok {
		aSpace.Unlock()
		return types.NotFoundErrorf("cannot find address pool for poolID:%s", poolID)
	}

	if address == nil {
		aSpace.Unlock()
		return types.BadRequestErrorf("invalid address: nil")
	}

	if !p.Pool.Contains(address) {
		aSpace.Unlock()
		return ipamapi.ErrIPOutOfRange
	}

	c := p
	for c.Range != nil {
		k = c.ParentKey
		c = aSpace.subnets[k]
	}
	aSpace.Unlock()

	mask := p.Pool.Mask



	h, err := types.GetHostPartIP(address, mask)
	if err != nil {
		return types.InternalErrorf("failed to release address %s: %v", address.String(), err)
	}

	bm, err := a.retrieveBitmask(k, c.Pool)
	if err != nil {
		return types.InternalErrorf("could not find bitmask in datastore for %s on address %v release from pool %s: %v",
			k.String(), address, poolID, err)
	}

	return bm.Unset(ipToUint64(h))
}

func (a *Allocator) getAddress(nw *net.IPNet, bitmask *bitseq.Handle, prefAddress net.IP, ipr *AddressRange) (net.IP, error) {
	var (
		ordinal uint64
		err     error
		base    *net.IPNet
	)

	base = types.GetIPNetCopy(nw)

	if bitmask.Unselected() <= 0 {
		return nil, ipamapi.ErrNoAvailableIPs
	}
	if ipr == nil && prefAddress == nil {
		ordinal, err = bitmask.SetAny()
	} else if prefAddress != nil {
		hostPart, e := types.GetHostPartIP(prefAddress, base.Mask)
		if e != nil {
			return nil, types.InternalErrorf("failed to allocate requested address %s: %v", prefAddress.String(), e)
		}
		ordinal = ipToUint64(types.GetMinimalIP(hostPart))
		err = bitmask.Set(ordinal)
	} else {
		ordinal, err = bitmask.SetAnyInRange(ipr.Start, ipr.End)
	}

	switch err {
	case nil:
		// Convert IP ordinal for this subnet into IP address
		return generateAddress(ordinal, base), nil
	case bitseq.ErrBitAllocated:
		return nil, ipamapi.ErrIPAlreadyAllocated
	case bitseq.ErrNoBitAvailable:
		return nil, ipamapi.ErrNoAvailableIPs
	default:
		return nil, err
	}
}

// DumpDatabase dumps the internal info
func (a *Allocator) DumpDatabase() string {
	a.Lock()
	aspaces := make(map[string]*addrSpace, len(a.addrSpaces))
	orderedAS := make([]string, 0, len(a.addrSpaces))
	for as, aSpace := range a.addrSpaces {
		orderedAS = append(orderedAS, as)
		aspaces[as] = aSpace
	}
	a.Unlock()

	sort.Strings(orderedAS)

	var s string
	for _, as := range orderedAS {
		aSpace := aspaces[as]
		s = fmt.Sprintf("\n\n%s Config", as)
		aSpace.Lock()
		for k, config := range aSpace.subnets {
			s = fmt.Sprintf("%s%s", s, fmt.Sprintf("\n%v: %v", k, config))
			if config.Range == nil {
				a.retrieveBitmask(k, config.Pool)
			}
		}
		aSpace.Unlock()
	}

	s = fmt.Sprintf("%s\n\nBitmasks", s)
	for k, bm := range a.addresses {
		s = fmt.Sprintf("%s%s", s, fmt.Sprintf("\n%s: %s", k, bm))
	}

	return s
}
