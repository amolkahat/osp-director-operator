package common

// NOTE: Most of the code here was forklifted from whereabouts-cni's allocation code.
// There is potential to refactor into a library perhaps useful to both projects?

import (
	"fmt"
	"math/big"
	"net"

	ospdirectorv1beta1 "github.com/openstack-k8s-operators/osp-director-operator/api/v1beta1"
)

// AssignmentError defines an IP assignment error.
type AssignmentError struct {
	firstIP net.IP
	lastIP  net.IP
	ipnet   net.IPNet
}

// AssignIPDetails -
type AssignIPDetails struct {
	IPnet      net.IPNet
	RangeStart net.IP
	RangeEnd   net.IP
	// RoleReservelist - Reservelist of all reservations of the role
	RoleReservelist []ospdirectorv1beta1.IPReservation
	// Reservelist - Reservelist of all reservations
	Reservelist   []ospdirectorv1beta1.IPReservation
	ExcludeRanges []string
	HostRef       string
	Hostname      string
	VIP           bool
	Deleted       bool
}

func (a AssignmentError) Error() string {
	return fmt.Sprintf("Could not allocate IP in range: ip: %v / - %v / range: %#v", a.firstIP, a.lastIP, a.ipnet)
}

// AssignIP assigns an IP using a range and a reserve list.
func AssignIP(assignIPDetails AssignIPDetails) (net.IPNet, []ospdirectorv1beta1.IPReservation, error) {

	newip, updatedreservelist, err := IterateForAssignment(assignIPDetails)
	if err != nil {
		return net.IPNet{}, nil, err
	}

	return net.IPNet{IP: newip, Mask: assignIPDetails.IPnet.Mask}, updatedreservelist, nil
}

// IterateForAssignment iterates given an IP/IPNet and a list of reserved IPs
func IterateForAssignment(assignIPDetails AssignIPDetails) (net.IP, []ospdirectorv1beta1.IPReservation, error) {

	firstip := assignIPDetails.RangeStart
	var lastip net.IP
	if assignIPDetails.RangeEnd != nil {
		lastip = assignIPDetails.RangeEnd
	} else {
		var err error
		firstip, lastip, err = GetIPRange(assignIPDetails.RangeStart, assignIPDetails.IPnet)
		if err != nil {
			//logging.Errorf("GetIPRange request failed with: %v", err)
			return net.IP{}, assignIPDetails.Reservelist, err
		}
	}
	//logging.Debugf("IterateForAssignment input >> ip: %v | ipnet: %v | first IP: %v | last IP: %v", rangeStart, ipnet, firstip, lastip)

	reserved := make(map[string]bool)
	for _, r := range assignIPDetails.Reservelist {
		ip := BigIntToIP(*IPToBigInt(net.ParseIP(r.IP)))
		reserved[ip.String()] = true
	}

	excluded := []*net.IPNet{}
	for _, v := range assignIPDetails.ExcludeRanges {
		_, subnet, _ := net.ParseCIDR(v)
		excluded = append(excluded, subnet)
	}

	// Iterate every IP address in the range
	var assignedip net.IP
	performedassignment := false
MAINITERATION:
	for i := IPToBigInt(firstip); IPToBigInt(lastip).Cmp(i) == 1 || IPToBigInt(lastip).Cmp(i) == 0; i.Add(i, big.NewInt(1)) {

		assignedip = BigIntToIP(*i)
		stringip := fmt.Sprint(assignedip)
		// For each address see if it has been allocated
		if reserved[stringip] {
			// Continue if this IP is allocated.
			continue
		}

		// We can try to work with the current IP
		// However, let's skip 0-based addresses
		// So go ahead and continue if the 4th/16th byte equals 0
		ipbytes := i.Bytes()
		if isIntIPv4(i) {
			if ipbytes[5] == 0 {
				continue
			}
		} else {
			if ipbytes[15] == 0 {
				continue
			}
		}

		// Lastly, we need to check if this IP is within the range of excluded subnets
		for _, subnet := range excluded {
			if subnet.Contains(BigIntToIP(*i).To16()) {
				continue MAINITERATION
			}
		}

		// Ok, this one looks like we can assign it!
		performedassignment = true

		assignIPDetails.RoleReservelist = append(assignIPDetails.RoleReservelist, ospdirectorv1beta1.IPReservation{
			Hostname: assignIPDetails.Hostname,
			IP:       assignedip.String(),
			VIP:      assignIPDetails.VIP,
			Deleted:  assignIPDetails.Deleted,
		})
		break
	}

	if !performedassignment {
		return net.IP{}, assignIPDetails.RoleReservelist, AssignmentError{firstip, lastip, assignIPDetails.IPnet}
	}

	return assignedip, assignIPDetails.RoleReservelist, nil
}

// GetIPRange returns the first and last IP in a range
func GetIPRange(ip net.IP, ipnet net.IPNet) (net.IP, net.IP, error) {

	// Good hints here: http://networkbit.ch/golang-ip-address-manipulation/
	// Nice info on bitwise operations: https://yourbasic.org/golang/bitwise-operator-cheat-sheet/
	// Get info about the mask.
	mask := ipnet.Mask
	ones, bits := mask.Size()
	masklen := bits - ones

	// Error when the mask isn't large enough.
	if masklen < 2 {
		return nil, nil, fmt.Errorf("net mask is too short, must be 2 or more: %v", masklen)
	}

	// Get a long from the current IP address
	longip := IPToBigInt(ip)

	// Shift out to get the lowest IP value.
	var lowestiplong big.Int
	lowestiplong.Rsh(longip, uint(masklen))
	lowestiplong.Lsh(&lowestiplong, uint(masklen))

	// Get the mask as a long, shift it out
	var masklong big.Int
	// We need to generate the largest number...
	// Let's try to figure out if it's IPv4 or v6
	var maxval big.Int
	if len(lowestiplong.Bytes()) == net.IPv6len {
		// It's v6
		// Maximum IPv6 value: 0xffffffffffffffffffffffffffffffff
		maxval.SetString("0xffffffffffffffffffffffffffffffff", 0)
	} else {
		// It's v4
		// Maximum IPv4 value: 4294967295
		maxval.SetUint64(4294967295)
	}

	masklong.Rsh(&maxval, uint(ones))

	// Now figure out the highest value...
	// We can OR that value...
	var highestiplong big.Int
	highestiplong.Or(&lowestiplong, &masklong)
	// remove network and broadcast address from the  range
	var incIP big.Int
	incIP.SetInt64(1)
	lowestiplong.Add(&lowestiplong, &incIP)   // fixes to remove network address
	highestiplong.Sub(&highestiplong, &incIP) //fixes to remove broadcast address

	// Convert to net.IPs
	firstip := BigIntToIP(lowestiplong)
	if lowestiplong.Cmp(longip) < 0 { // if range_start was provided and its greater.
		firstip = BigIntToIP(*longip)
	}
	lastip := BigIntToIP(highestiplong)

	return firstip, lastip, nil

}

func isIntIPv4(checkipint *big.Int) bool {
	return !(len(checkipint.Bytes()) == net.IPv6len)
}

// BigIntToIP converts a big.Int to a net.IP
func BigIntToIP(inipint big.Int) net.IP {
	outip := net.IP(make([]byte, net.IPv6len))
	intbytes := inipint.Bytes()
	if len(intbytes) == net.IPv6len {
		// This is an IPv6 address.
		copy(outip, intbytes)
	} else {
		// It's an IPv4 address.
		for i := 0; i < len(intbytes); i++ {
			outip[i+10] = intbytes[i]
		}
	}
	return outip
}

// IPToBigInt converts a net.IP to a big.Int
func IPToBigInt(IPv6Addr net.IP) *big.Int {
	IPv6Int := big.NewInt(0)
	IPv6Int.SetBytes(IPv6Addr)
	return IPv6Int
}

// GetCidrParts - returns addr and cidr suffix
func GetCidrParts(cidr string) (string, int, error) {
	ipAddr, net, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0, err
	}

	cidrSuffix, _ := net.Mask.Size()

	return ipAddr.String(), cidrSuffix, nil
}
