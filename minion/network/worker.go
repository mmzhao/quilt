package network

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/NetSys/quilt/db"
	"github.com/NetSys/quilt/join"
	"github.com/NetSys/quilt/minion/docker"
	"github.com/NetSys/quilt/minion/ovsdb"
	"github.com/NetSys/quilt/minion/supervisor"
	"github.com/NetSys/quilt/stitch"
	"github.com/NetSys/quilt/util"

	log "github.com/Sirupsen/logrus"
)

const (
	nsPath           string = "/var/run/netns"
	innerVeth        string = "eth0"
	loopback         string = "lo"
	innerMTU         int    = 1400
	concurrencyLimit int    = 32 // Adjust to change per function goroutine limit
)

// The machine's public interface.
var publicInterface string

// This represents a network namespace
type nsInfo struct {
	ns  string
	pid int
}

type nsInfoSlice []nsInfo

// This represents a network device
type netdev struct {
	// These apply to all links
	name string
	up   bool

	// These only apply to veths
	peerNS  string
	peerMTU int
}

type netdevSlice []netdev

// This represents a route in the routing table
type route struct {
	ip        string
	dev       string
	isDefault bool
}

type routeSlice []route

// This represents a rule in the iptables
type ipRule struct {
	cmd   string
	chain string
	opts  string // Must be sorted - see makeIPRule
}

type ipRuleSlice []ipRule

// OFRule is an OpenFlow rule for Open vSwitch's OpenFlow table.
type OFRule struct {
	table   string
	match   string
	actions string
}

// OFRuleSlice is an alias for []OFRule to allow for joins
type OFRuleSlice []OFRule

// Query the database for any running containers and for each container running on this
// host, do the following:
//    - Create a pair of virtual interfaces for the container if it's new and
//      assign them the appropriate addresses
//    - Move one of the interfaces into the network namespace of the container,
//      and assign it the MAC and IP addresses from OVN
//    - Attach the other interface to the OVS bridge quilt-int
//    - Attach this container to the logical network by creating a pair of OVS
//      patch ports between br-int and quilt-int, then install flows to send traffic
//      between the patch port on quilt-int and the container's outer interface
//      (These flows live in Table 2)
//    - Update the container's /etc/hosts file with the set of labels it may access.
//    - Populate quilt-int with the OpenFlow rules necessary to facilitate forwarding.
//
// To connect to the public internet, we do the following setup:
//    - On the host:
//        * Bring up the quilt-int device and assign it the IP address 10.0.0.1/8, and
//          the corresponding MAC address.
//          quilt-int is the containers' default gateway.
//        * Set up NAT for packets coming from the 10/8 subnet and leaving on eth0.
//    - On each container:
//        * Make eth0 the route to the 10/8 subnet.
//        * Make the quilt-int device on the host the default gateway (this is the LOCAL
//          port on the quilt-int bridge).
//        * Setup /etc/resolv.conf with the same nameservers as the host.
//    - On the quilt-int bridge:
//        * Forward packets from containers to LOCAL, if their dst MAC is that of the
//          default gateway.
//        * Forward arp packets to both br-int and the default gateway.
//        * Forward packets from LOCAL to the container with the packet's dst MAC.

func runWorker(conn db.Conn, dk docker.Client) {
	minion, err := conn.MinionSelf()
	if err != nil || minion.Role != db.Worker {
		return
	}

	odb, err := ovsdb.Open()
	if err != nil {
		log.Warning("Failed to connect to ovsdb-server: %s", err)
		return
	}
	defer odb.Close()

	if publicInterface == "" {
		if pubIntf, err := getPublicInterface(); err == nil {
			publicInterface = pubIntf
		} else {
			log.WithError(err).Error("Failed to get public interface")
		}
	}

	// XXX: By doing all the work within a transaction, we (kind of) guarantee that
	// containers won't be removed while we're in the process of setting them up.
	// Not ideal, but for now it's good enough.
	conn.Transact(func(view db.Database) error {
		containers := view.SelectFromContainer(func(c db.Container) bool {
			return c.DockerID != "" && c.IP != "" && c.Mac != "" &&
				c.Pid != 0
		})
		labels := view.SelectFromLabel(func(l db.Label) bool {
			return l.IP != ""
		})
		connections := view.SelectFromConnection(nil)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			updateEtcHosts(dk, containers, labels, connections)
			wg.Done()
		}()

		go func() {
			updateNameservers(dk, containers)
			wg.Done()
		}()

		updateNamespaces(containers)
		updateVeths(containers)
		if publicInterface != "" {
			updateNAT(publicInterface, containers, connections)
		}
		updatePorts(odb, containers)

		if exists, err := linkExists("", quiltBridge); exists {
			updateDefaultGw(odb)
			wg.Add(1)
			go func() {
				updateOpenFlow(dk, odb, containers, labels, connections)
				wg.Done()
			}()
		} else if err != nil {
			log.WithError(err).Error("failed to check if link exists")
		}

		updateContainerIPs(containers, labels)
		updateRoutes(containers)

		wg.Wait()
		updateLoopback(containers)
		return nil
	})
}

// If a namespace in the path is detected as invalid and conflicts with
// a namespace that should exist, it's removed and replaced.
func updateNamespaces(containers []db.Container) {
	// A symbolic link in the netns path is considered a "namespace".
	// The actual namespace is elsewhere but we link them all into the
	// canonical location and manage them there.
	//
	// We keep all our namespaces in /var/run/netns/

	var targetNamespaces nsInfoSlice
	for _, dbc := range containers {
		targetNamespaces = append(targetNamespaces,
			nsInfo{ns: networkNS(dbc.DockerID), pid: dbc.Pid})
	}
	currentNamespaces, err := generateCurrentNamespaces()
	if err != nil {
		log.WithError(err).Error("failed to get namespaces")
		return
	}

	key := func(val interface{}) interface{} {
		return val.(nsInfo).ns
	}

	_, lefts, rights := join.HashJoin(currentNamespaces, targetNamespaces, key, key)

	for _, l := range lefts {
		if err := delNS(l.(nsInfo)); err != nil {
			log.WithError(err).Error("error deleting namespace")
		}
	}

	for _, r := range rights {
		if err := addNS(r.(nsInfo)); err != nil {
			log.WithError(err).Error("error adding namespace")
		}
	}
}

func generateCurrentNamespaces() (nsInfoSlice, error) {
	files, err := ioutil.ReadDir(nsPath)
	if err != nil {
		return nil, err
	}

	var infos nsInfoSlice
	for _, file := range files {
		fi, err := os.Lstat(fmt.Sprintf("%s/%s", nsPath, file.Name()))
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			infos = append(infos, nsInfo{ns: file.Name()})
		}
	}
	return infos, nil
}

func delNS(info nsInfo) error {
	netnsDst := fmt.Sprintf("%s/%s", nsPath, info.ns)
	if err := os.Remove(netnsDst); err != nil {
		return fmt.Errorf("failed to remove namespace %s: %s",
			netnsDst, err)
	}
	return nil
}

func addNS(info nsInfo) error {
	netnsSrc := fmt.Sprintf("/hostproc/%d/ns/net", info.pid)
	netnsDst := fmt.Sprintf("%s/%s", nsPath, info.ns)
	if _, err := os.Stat(netnsDst); err == nil {
		if err := os.Remove(netnsDst); err != nil {
			return fmt.Errorf("failed to remove broken namespace %s: %s",
				netnsDst, err)
		}
	} else if !os.IsNotExist(err) && err != nil {
		return fmt.Errorf("failed to query namespace %s: %s",
			netnsDst, err)
	}
	if err := os.Symlink(netnsSrc, netnsDst); err != nil {
		return fmt.Errorf("failed to create namespace %s with source %s: %s",
			netnsDst, netnsSrc, err)
	}
	return nil
}

func updateLoopback(containers []db.Container) {
	for _, dbc := range containers {
		namespace := networkNS(dbc.DockerID)
		if err := upLink(namespace, loopback); err != nil {
			log.WithError(err).Error("failed to up loopback device")
		}
	}
}

func updateVeths(containers []db.Container) {
	// A virtual ethernet link that links the host and container is a "veth".
	//
	// The ends of the veth have different config options like mtu, etc.
	// However if you delete one side, both will be deleted.

	targetVeths := generateTargetVeths(containers)
	currentVeths, err := generateCurrentVeths(containers)
	if err != nil {
		log.WithError(err).Error("failed to get veths")
		return
	}

	key := func(val interface{}) interface{} {
		return val.(netdev).name
	}

	pairs, lefts, rights := join.HashJoin(currentVeths, targetVeths, key, key)

	// Changing veths takes a long time, so we do it concurrently
	doVeths(lefts, delVeth, "delete")
	doVeths(rights, addVeth, "add")
	for _, p := range pairs {
		if err := modVeth(p.L.(netdev), p.R.(netdev)); err != nil {
			log.WithError(err).Error("failed to modify veth")
			continue
		}
	}
}

func doVeths(veths []interface{}, do func(netdev) error, action string) {
	var wg sync.WaitGroup
	vethsChannel := make(chan netdev, len(veths))
	for _, v := range veths {
		vethsChannel <- v.(netdev)
	}
	close(vethsChannel)

	wg.Add(concurrencyLimit)
	for i := 0; i < concurrencyLimit; i++ {
		go func() {
			defer wg.Done()
			for v := range vethsChannel {
				if err := do(v); err != nil {
					log.WithError(err).Errorf("failed to %s veth",
						action)
				}
			}
		}()
	}
	wg.Wait()
}

func generateTargetVeths(containers []db.Container) netdevSlice {
	var configs netdevSlice
	for _, dbc := range containers {
		_, vethOut := veths(dbc.DockerID)
		cfg := netdev{
			name:    vethOut,
			up:      true,
			peerNS:  networkNS(dbc.DockerID),
			peerMTU: innerMTU,
		}
		configs = append(configs, cfg)
	}
	return configs
}

func generateCurrentVeths(containers []db.Container) (netdevSlice, error) {
	names, err := listVeths()
	if err != nil {
		return nil, err
	}

	var configs netdevSlice
	for _, name := range names {
		cfg := netdev{
			name: name,
		}

		iface, err := net.InterfaceByName(name)
		if err != nil {
			log.WithFields(log.Fields{
				"name":  name,
				"error": err,
			}).Error("failed to get interface")
			continue
		}

		for _, dbc := range containers {
			_, vethOut := veths(dbc.DockerID)
			if vethOut == name {
				cfg.peerNS = networkNS(dbc.DockerID)
				break
			}
		}
		if cfg.peerNS != "" {
			if nsExists, err := namespaceExists(cfg.peerNS); err != nil {
				log.WithFields(log.Fields{
					"namespace": cfg.peerNS,
					"error":     err,
				}).Error("error while searching for namespace")
				continue
			} else if nsExists {
				lkExists, err := linkExists(cfg.peerNS, innerVeth)
				if err != nil {
					log.WithFields(log.Fields{
						"namespace": cfg.peerNS,
						"link":      innerVeth,
						"error":     err,
					}).Error("error while checking " +
						"whether link exists in namespace")
					continue
				} else if lkExists {
					cfg.peerMTU, err = getLinkMTU(cfg.peerNS,
						innerVeth)
					if err != nil {
						log.WithError(err).Error(
							"failed to get link mtu")
						continue
					}
				}
			}
		}

		cfg.up = (iface.Flags&net.FlagUp == net.FlagUp)
		configs = append(configs, cfg)
	}
	return configs, nil
}

func updateNAT(publicInterface string, containers []db.Container,
	connections []db.Connection) {

	targetRules := generateTargetNatRules(publicInterface, containers, connections)
	currRules, err := generateCurrentNatRules()
	if err != nil {
		log.WithError(err).Error("failed to get NAT rules")
		return
	}

	_, rulesToDel, rulesToAdd := join.HashJoin(currRules, targetRules, nil, nil)

	for _, rule := range rulesToDel {
		if err := deleteNatRule(rule.(ipRule)); err != nil {
			log.WithError(err).Error("failed to delete ip rule")
			continue
		}
	}

	for _, rule := range rulesToAdd {
		if err := addNatRule(rule.(ipRule)); err != nil {
			log.WithError(err).Error("failed to add ip rule")
			continue
		}
	}
}

func generateCurrentNatRules() (ipRuleSlice, error) {
	stdout, _, err := shVerbose("iptables -t nat -S")
	if err != nil {
		return nil, fmt.Errorf("failed to get IP tables: %s", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	var rules ipRuleSlice

	for scanner.Scan() {
		line := scanner.Text()

		rule, err := makeIPRule(line)
		if err != nil {
			return nil, fmt.Errorf("failed to get current IP rules: %s", err)
		}
		rules = append(rules, rule)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error while getting IP tables: %s", err)
	}
	return rules, nil
}

func generateTargetNatRules(publicInterface string, containers []db.Container,
	connections []db.Connection) ipRuleSlice {
	strRules := []string{
		"-P PREROUTING ACCEPT",
		"-P INPUT ACCEPT",
		"-P OUTPUT ACCEPT",
		"-P POSTROUTING ACCEPT",
		fmt.Sprintf("-A POSTROUTING -s 10.0.0.0/8 -o %s -j MASQUERADE",
			publicInterface),
	}

	protocols := []string{"tcp", "udp"}
	// Map each container IP to all ports on which it can receive packets
	// from the public internet.
	portsFromWeb := make(map[string]map[int]struct{})

	for _, dbc := range containers {
		for _, conn := range connections {

			if conn.From != stitch.PublicInternetLabel {
				continue
			}

			for _, l := range dbc.Labels {

				if conn.To != l {
					continue
				}

				if _, ok := portsFromWeb[dbc.IP]; !ok {
					portsFromWeb[dbc.IP] = make(map[int]struct{})
				}

				portsFromWeb[dbc.IP][conn.MinPort] = struct{}{}
			}
		}
	}

	// Map the container's port to the same port of the host.
	for ip, ports := range portsFromWeb {
		for port := range ports {
			for _, protocol := range protocols {
				strRules = append(strRules, fmt.Sprintf(
					"-A PREROUTING -i %[1]s "+
						"-p %[2]s -m %[2]s --dport %[3]d -j "+
						"DNAT --to-destination %[4]s:%[3]d",
					publicInterface, protocol, port, ip))
			}
		}
	}

	var rules ipRuleSlice
	for _, r := range strRules {
		rule, err := makeIPRule(r)
		if err != nil {
			panic("malformed target NAT rule")
		}
		rules = append(rules, rule)
	}
	return rules
}

// There certain exceptions, as certain ports will never be deleted.
func updatePorts(odb ovsdb.Client, containers []db.Container) {
	// An Open vSwitch patch port is referred to as a "port".
	targetPorts := generateTargetPorts(containers)
	currentPorts, err := odb.ListInterfaces()
	if err != nil {
		log.WithError(err).Error("failed to generate current openflow ports")
		return
	}

	key := func(val interface{}) interface{} {
		return struct {
			name, bridge string
		}{
			name:   val.(ovsdb.Interface).Name,
			bridge: val.(ovsdb.Interface).Bridge,
		}
	}

	pairs, lefts, rights := join.HashJoin(ovsdb.InterfaceSlice(currentPorts),
		targetPorts, key, key)

	for _, l := range lefts {
		if l.(ovsdb.Interface).Type == ovsdb.InterfaceTypeGeneve ||
			l.(ovsdb.Interface).Type == ovsdb.InterfaceTypeInternal {
			// The "bridge" port and the geneve port should never be deleted.
			continue
		}
		if err := odb.DeleteInterface(l.(ovsdb.Interface)); err != nil {
			log.WithError(err).Error("failed to delete openflow port")
			continue
		}
	}
	for _, r := range rights {
		if err := addPort(odb, r.(ovsdb.Interface)); err != nil {
			log.WithError(err).Error("failed to add openflow port")
			continue
		}
	}
	for _, p := range pairs {
		l := p.L.(ovsdb.Interface)
		r := p.R.(ovsdb.Interface)
		if err := modPort(odb, l, r); err != nil {
			log.WithError(err).Error("failed to modify openflow port")
			continue
		}
	}
}

func generateTargetPorts(containers []db.Container) ovsdb.InterfaceSlice {
	var configs ovsdb.InterfaceSlice
	for _, dbc := range containers {
		_, vethOut := veths(dbc.DockerID)
		peerBr, peerQuilt := patchPorts(dbc.DockerID)
		configs = append(configs, ovsdb.Interface{
			Name:   vethOut,
			Bridge: quiltBridge,
		})
		configs = append(configs, ovsdb.Interface{
			Name:   peerQuilt,
			Bridge: quiltBridge,
			Type:   ovsdb.InterfaceTypePatch,
			Peer:   peerBr,
		})
		configs = append(configs, ovsdb.Interface{
			Name:        peerBr,
			Bridge:      ovnBridge,
			Type:        ovsdb.InterfaceTypePatch,
			Peer:        peerQuilt,
			AttachedMAC: dbc.Mac,
			IfaceID:     dbc.IP,
		})
	}
	return configs
}

func addPort(odb ovsdb.Client, iface ovsdb.Interface) error {
	if err := odb.CreateInterface(iface.Bridge, iface.Name); err != nil {
		log.WithError(err).Warning("error creating openflow port")
	}
	return odb.ModifyInterface(iface)
}

func modPort(odb ovsdb.Client, current ovsdb.Interface, target ovsdb.Interface) error {
	if current.Type != target.Type ||
		current.Peer != target.Peer ||
		current.AttachedMAC != target.AttachedMAC ||
		current.IfaceID != target.IfaceID {
		return odb.ModifyInterface(target)
	}

	return nil
}

func updateDefaultGw(odb ovsdb.Client) {
	currMac, err := getMac("", quiltBridge)
	if err != nil {
		log.WithError(err).Errorf("failed to get MAC for %s", quiltBridge)
		return
	}

	if currMac != gatewayMAC {
		if err := odb.SetBridgeMac(quiltBridge, gatewayMAC); err != nil {
			log.WithError(err).Error("failed to set MAC for default gateway")
		}
	}

	if err := upLink("", quiltBridge); err != nil {
		log.WithError(err).Error("failed to up default gateway")
	}

	currIPs, err := listIP("", quiltBridge)
	if err != nil {
		log.WithError(err).Errorf("failed to list IPs")
		return
	}

	targetIPs := []string{GatewayIP + "/8"}
	if err := updateIPs("", quiltBridge, currIPs, targetIPs); err != nil {
		log.WithError(err).Errorf("failed to update IPs")
	}
}

func updateIPs(namespace string, dev string, currIPs []string,
	targetIPs []string) error {

	_, ipToDel, ipToAdd := join.HashJoin(join.StringSlice(currIPs),
		join.StringSlice(targetIPs), nil, nil)

	for _, ip := range ipToDel {
		if err := delIP(namespace, ip.(string), dev); err != nil {
			return err
		}
	}

	for _, ip := range ipToAdd {
		if err := addIP(namespace, ip.(string), dev); err != nil {
			return err
		}
	}

	return nil
}

func updateContainerIPs(containers []db.Container, labels []db.Label) {
	labelIP := make(map[string]string)
	for _, l := range labels {
		labelIP[l.Label] = l.IP
	}

	containerChannel := make(chan db.Container)
	var wg sync.WaitGroup
	wg.Add(concurrencyLimit)
	for i := 0; i < concurrencyLimit; i++ {
		go func() {
			updateContainers(containerChannel, labelIP)
			wg.Done()
		}()
	}

	for _, c := range containers {
		containerChannel <- c
	}
	close(containerChannel)

	wg.Wait()
}

func updateContainers(in chan db.Container, labelIP map[string]string) {
	for dbc := range in {
		var err error
		ns := networkNS(dbc.DockerID)
		ip := dbc.IP

		currIPs, err := listIP(ns, innerVeth)
		if err != nil {
			log.WithError(err).Error("failed to list current ip addresses")
			continue
		}

		newIPSet := make(map[string]struct{})
		newIPSet[ip] = struct{}{}
		for _, l := range dbc.Labels {
			newIP := labelIP[l]
			if newIP != "" {
				newIPSet[newIP] = struct{}{}
			}
		}

		var newIPs []string
		for ip := range newIPSet {
			newIPs = append(newIPs, ip+"/8")
		}

		if err := updateIPs(ns, innerVeth, currIPs, newIPs); err != nil {
			log.WithError(err).Error("failed to update IPs")
			continue
		}

		currMac, err := getMac(ns, innerVeth)
		if err != nil {
			log.WithError(err).Errorf("failed to get MAC for %s in %s",
				innerVeth, namespaceName(ns))
			continue
		}

		if currMac != dbc.Mac {
			if err := setMac(ns, innerVeth, dbc.Mac); err != nil {
				log.WithError(err).Errorf(
					"failed to set MAC for %s in %s",
					innerVeth, namespaceName(ns))
				continue
			}
		}
	}
}

func updateRoutes(containers []db.Container) {
	targetRoutes := routeSlice{
		{
			ip:        "10.0.0.0/8",
			dev:       innerVeth,
			isDefault: false,
		},
		{
			ip:        GatewayIP,
			dev:       innerVeth,
			isDefault: true,
		},
	}

	for _, dbc := range containers {
		ns := networkNS(dbc.DockerID)

		currentRoutes, err := generateCurrentRoutes(ns)
		if err != nil {
			log.WithError(err).Error("failed to get current ip routes")
			continue
		}

		_, routesDel, routesAdd := join.HashJoin(currentRoutes, targetRoutes,
			nil, nil)

		for _, l := range routesDel {
			if err := deleteRoute(ns, l.(route)); err != nil {
				log.WithError(err).Error("error deleting route")
			}
		}

		for _, r := range routesAdd {
			if err := addRoute(ns, r.(route)); err != nil {
				log.WithError(err).Error("error adding route")
			}
		}
	}
}

func generateCurrentRoutes(namespace string) (routeSlice, error) {
	stdout, _, err := ipExecVerbose(namespace, "route show")
	if err != nil {
		return nil, fmt.Errorf("failed to get routes in %s: %s",
			namespaceName(namespace), err)
	}

	var routes routeSlice
	routeRE := regexp.MustCompile(
		"((?:[0-9]{1,3}\\.){3}[0-9]{1,3}/[0-9]{1,2})\\sdev\\" +
			"s(\\S+)")
	gwRE := regexp.MustCompile(
		"default via ((?:[0-9]{1,3}\\.){3}[0-9]{1,3}) dev (\\S+)")
	for _, r := range routeRE.FindAllSubmatch(stdout, -1) {
		routes = append(routes, route{
			ip:        string(r[1]),
			dev:       string(r[2]),
			isDefault: false,
		})
	}

	for _, r := range gwRE.FindAllSubmatch(stdout, -1) {
		routes = append(routes, route{
			ip:        string(r[1]),
			dev:       string(r[2]),
			isDefault: true,
		})
	}

	return routes, nil
}

// Sets up the OpenFlow tables to get packets from containers into the OVN controlled
// bridge.  The Openflow tables are organized as follows.
//
//     - Table 0 will check for packets destined to an ip address of a label with MAC
//     0A:00:00:00:00:00 (obtained by OVN faking out arp) and use the OF multipath action
//     to balance load packets across n links where n is the number of containers
//     implementing the label.  This result is stored in NXM_NX_REG0. This is done using
//     a symmetric l3/4 hash, so transport connections should remain intact.
//
//     -Table 1 reads NXM_NX_REG0 and changes the destination mac address to one of the
//     MACs of the containers that implement the label
//
// XXX: The multipath action doesn't perform well.  We should migrate away from it
// choosing datapath recirculation instead.
func updateOpenFlow(dk docker.Client, odb ovsdb.Client, containers []db.Container,
	labels []db.Label, connections []db.Connection) {

	targetOF, err := generateTargetOpenFlow(dk, odb, containers, labels, connections)
	if err != nil {
		log.WithError(err).Error("failed to get target OpenFlow flows")
		return
	}
	currentOF, err := generateCurrentOpenFlow(dk)
	if err != nil {
		log.WithError(err).Error("failed to get current OpenFlow flows")
		return
	}

	_, flowsToDel, flowsToAdd := join.HashJoin(currentOF, targetOF, nil, nil)

	if err := deleteOFRules(dk, flowsToDel); err != nil {
		log.WithError(err).Error("error deleting OpenFlow flow")
	}

	if err := addOFRules(dk, flowsToAdd); err != nil {
		log.WithError(err).Error("error adding OpenFlow flow")
	}
}

func generateCurrentOpenFlow(dk docker.Client) (OFRuleSlice, error) {
	args := "ovs-ofctl dump-flows " + quiltBridge
	stdout, err := dk.ExecVerbose(supervisor.Ovsvswitchd,
		strings.Split(args, " ")...)

	if err != nil {
		return nil, fmt.Errorf("failed to list OpenFlow flows: %s",
			string(stdout))
	}

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	var flows OFRuleSlice

	// The first line isn't a flow, so skip it.
	scanner.Scan()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		flow, err := makeOFRule(line)

		if err != nil {
			return nil, fmt.Errorf("failed to make OpenFlow rule: %s", err)
		}

		flows = append(flows, flow)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error while getting OpenFlow flows: %s",
			err)
	}

	return flows, nil
}

// The target flows must be in the same format as the output from ovs-ofctl
// dump-flows. To achieve this, we have some rather ugly hacks that handle
// a few special cases.
func generateTargetOpenFlow(dk docker.Client, odb ovsdb.Client,
	containers []db.Container, labels []db.Label,
	connections []db.Connection) (OFRuleSlice, error) {

	dflGatewayMAC, err := getMac("", quiltBridge)
	if err != nil {
		log.WithError(err).Error("failed to get MAC of default gateway.")
	}

	ifaces, err := odb.ListInterfaces()
	if err != nil {
		return nil, err
	}

	ifaceMap := make(map[string]int)
	for _, iface := range ifaces {
		if iface.OFPort != nil {
			ifaceMap[iface.Name] = *iface.OFPort
		}
	}

	var rules []string
	for _, dbc := range containers {
		_, vethOut := veths(dbc.DockerID)
		_, peerQuilt := patchPorts(dbc.DockerID)
		dbcMac := dbc.Mac

		ofQuilt, ok := ifaceMap[peerQuilt]
		if !ok {
			continue
		}

		ofVeth, ok := ifaceMap[vethOut]
		if !ok {
			continue
		}

		if ofQuilt < 0 || ofVeth < 0 {
			continue
		}

		rules = append(rules, []string{
			fmt.Sprintf("table=0 priority=%d,in_port=%d "+
				"actions=output:%d", 5000, ofQuilt, ofVeth),
			fmt.Sprintf("table=2 priority=%d,in_port=%d "+
				"actions=output:%d", 5000, ofVeth, ofQuilt),
			fmt.Sprintf("table=0 priority=%d,in_port=%d "+
				"actions=output:%d", 0, ofVeth, ofQuilt),
		}...)

		protocols := []string{"tcp", "udp"}

		portsToWeb := make(map[int]struct{})
		portsFromWeb := make(map[int]struct{})
		for _, l := range dbc.Labels {
			for _, conn := range connections {
				if conn.From == l &&
					conn.To == stitch.PublicInternetLabel {
					portsToWeb[conn.MinPort] = struct{}{}
				} else if conn.From ==
					stitch.PublicInternetLabel && conn.To == l {
					portsFromWeb[conn.MinPort] = struct{}{}
				}
			}
		}

		// LOCAL is the default quilt-int port created with the bridge.
		egressRule := fmt.Sprintf("table=0 priority=%d,in_port=%d,",
			5000, ofVeth) +
			"%s,%s," + fmt.Sprintf("dl_dst=%s actions=LOCAL", dflGatewayMAC)
		ingressRule := fmt.Sprintf("table=0 priority=%d,in_port=LOCAL,", 5000) +
			"%s,%s," + fmt.Sprintf("dl_dst=%s actions=%d", dbcMac, ofVeth)

		for port := range portsFromWeb {
			for _, protocol := range protocols {
				egressPort := fmt.Sprintf("tp_src=%d", port)
				rules = append(rules, fmt.Sprintf(egressRule, protocol,
					egressPort))

				ingressPort := fmt.Sprintf("tp_dst=%d", port)
				rules = append(rules, fmt.Sprintf(ingressRule, protocol,
					ingressPort))
			}
		}

		for port := range portsToWeb {
			for _, protocol := range protocols {
				egressPort := fmt.Sprintf("tp_dst=%d", port)
				rules = append(rules, fmt.Sprintf(egressRule, protocol,
					egressPort))

				ingressPort := fmt.Sprintf("tp_src=%d", port)
				rules = append(rules, fmt.Sprintf(ingressRule, protocol,
					ingressPort))
			}
		}

		var arpDst string
		if len(portsToWeb) > 0 || len(portsFromWeb) > 0 {
			// Allow ICMP
			rules = append(rules,
				fmt.Sprintf(
					"table=0 priority=%d,icmp,in_port=%d,dl_dst=%s"+
						" actions=LOCAL",
					5000, ofVeth, dflGatewayMAC))
			rules = append(rules,
				fmt.Sprintf(
					"table=0 priority=%d,icmp,in_port=LOCAL,"+
						"dl_dst=%s actions=output:%d",
					5000, dbcMac, ofVeth))

			arpDst = fmt.Sprintf("%d,LOCAL", ofQuilt)
		} else {
			arpDst = fmt.Sprintf("%d", ofQuilt)
		}

		if len(portsFromWeb) > 0 {
			// Allow default gateway to ARP for containers
			rules = append(rules, fmt.Sprintf(
				"table=0 priority=%d,arp,in_port=LOCAL,"+
					"dl_dst=ff:ff:ff:ff:ff:ff actions=output:%d",
				4500, ofVeth))
		}

		rules = append(rules, fmt.Sprintf(
			"table=0 priority=%d,arp,in_port=%d "+
				"actions=output:%s",
			4500, ofVeth, arpDst))
		rules = append(rules, fmt.Sprintf(
			"table=0 priority=%d,arp,in_port=LOCAL,"+
				"dl_dst=%s actions=output:%d",
			4500, dbcMac, ofVeth))
	}

	LabelMacs := make(map[string]map[string]struct{})
	for _, dbc := range containers {
		for _, l := range dbc.Labels {
			if _, ok := LabelMacs[l]; !ok {
				LabelMacs[l] = make(map[string]struct{})
			}
			LabelMacs[l][dbc.Mac] = struct{}{}
		}
	}

	for _, label := range labels {
		if !label.MultiHost {
			continue
		}

		macs := LabelMacs[label.Label]
		if len(macs) == 0 {
			continue
		}

		ip := label.IP
		n := len(macs)
		lg2n := int(math.Ceil(math.Log2(float64(n))))

		nxmRange := fmt.Sprintf("0..%d", lg2n)
		if lg2n == 0 {
			// dump-flows collapses 0..0 to just 0.
			nxmRange = "0"
		}
		mpa := fmt.Sprintf("multipath(symmetric_l3l4,0,modulo_n,%d,0,"+
			"NXM_NX_REG0[%s])", n, nxmRange)

		rules = append(rules, fmt.Sprintf(
			"table=0 priority=%d,dl_dst=%s,ip,nw_dst=%s "+
				"actions=%s,resubmit(,1)",
			4000, labelMac, ip, mpa))

		// We need the order to make diffing consistent.
		macList := make([]string, 0, n)
		for mac := range macs {
			macList = append(macList, mac)
		}
		sort.Strings(macList)

		i := 0
		regPrefix := ""
		for _, mac := range macList {
			if i > 0 {
				// dump-flows puts a 0x prefix for all register values
				// except for 0.
				regPrefix = "0x"
			}
			reg0 := fmt.Sprintf("%s%x", regPrefix, i)

			rules = append(rules, fmt.Sprintf(
				"table=1 priority=5000,ip,nw_dst=%s,"+
					"reg0=%s actions=mod_dl_dst:%s,resubmit(,2)",
				ip, reg0, mac))
			i++
		}
	}

	var targetRules OFRuleSlice
	for _, r := range rules {
		rule, err := makeOFRule(r)
		if err != nil {
			return nil, fmt.Errorf("failed to make OpenFlow rule: %s", err)
		}
		targetRules = append(targetRules, rule)
	}

	return targetRules, nil
}

// updateNameservers assigns each container the same nameservers as the host.
func updateNameservers(dk docker.Client, containers []db.Container) {
	hostResolv, err := ioutil.ReadFile("/etc/resolv.conf")
	if err != nil {
		log.WithError(err).Error("failed to read /etc/resolv.conf")
	}

	nsRE := regexp.MustCompile("nameserver\\s([0-9]{1,3}\\.){3}[0-9]{1,3}\\s+")
	matches := nsRE.FindAllString(string(hostResolv), -1)
	newNameservers := strings.Join(matches, "\n")

	containerChan := make(chan db.Container)
	var wg sync.WaitGroup
	wg.Add(concurrencyLimit)
	for i := 0; i < concurrencyLimit; i++ {
		go func() {
			updateNS(dk, newNameservers, containerChan)
			wg.Done()
		}()
	}

	for _, dbc := range containers {
		containerChan <- dbc
	}
	close(containerChan)
	wg.Wait()
}

func updateNS(dk docker.Client, newNameservers string, in chan db.Container) {
	for dbc := range in {
		id := dbc.DockerID
		currNameservers, err := dk.GetFromContainer(id,
			"/etc/resolv.conf")
		if err != nil {
			log.WithError(err).Error("failed to get " +
				"/etc/resolv.conf")
			continue
		}

		if newNameservers != currNameservers {
			err = dk.WriteToContainer(id, newNameservers, "/etc",
				"resolv.conf", 0644)
			if err != nil {
				log.WithError(err).Error(
					"failed to update /etc/resolv.conf")
			}
		}
	}
}

func updateEtcHosts(dk docker.Client, containers []db.Container, labels []db.Label,
	connections []db.Connection) {

	/* Map label name to the label itself. */
	labelMap := make(map[string]db.Label)

	/* Map label to a list of all labels it connect to. */
	conns := make(map[string][]string)

	for _, l := range labels {
		labelMap[l.Label] = l
	}

	for _, conn := range connections {
		if conn.To == stitch.PublicInternetLabel ||
			conn.From == stitch.PublicInternetLabel {
			continue
		}
		conns[conn.From] = append(conns[conn.From], conn.To)
	}

	containerChannel := make(chan db.Container)
	updateHosts := func(in chan db.Container) {
		for dbc := range in {
			id := dbc.DockerID
			currHosts, err := dk.GetFromContainer(id, "/etc/hosts")
			if err != nil {
				log.WithError(err).Error("Failed to get /etc/hosts")
				continue
			}

			newHosts := generateEtcHosts(dbc, labelMap, conns)

			if newHosts != currHosts {
				err = dk.WriteToContainer(id, newHosts, "/etc",
					"hosts", 0644)
				if err != nil {
					log.WithError(err).Error("Failed to update " +
						"/etc/hosts")
				}
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(concurrencyLimit)
	for i := 0; i < concurrencyLimit; i++ {
		go func() {
			updateHosts(containerChannel)
			wg.Done()
		}()
	}

	for _, dbc := range containers {
		containerChannel <- dbc
	}
	close(containerChannel)
	wg.Wait()
}

func generateEtcHosts(dbc db.Container, labels map[string]db.Label,
	conns map[string][]string) string {

	type entry struct {
		ip, host string
	}

	localhosts := []entry{
		{"127.0.0.1", "localhost"},
		{"::1", "localhost ip6-localhost ip6-loopback"},
		{"fe00::0", "ip6-localnet"},
		{"ff00::0", "ip6-mcastprefix"},
		{"ff02::1", "ip6-allnodes"},
		{"ff02::2", "ip6-allrouters"},
	}

	if dbc.IP != "" && dbc.DockerID != "" {
		entry := entry{dbc.IP, util.ShortUUID(dbc.DockerID)}
		localhosts = append(localhosts, entry)
	}

	newHosts := make(map[entry]struct{})
	for _, entry := range localhosts {
		newHosts[entry] = struct{}{}
	}

	for _, l := range dbc.Labels {
		for _, toLabel := range conns[l] {
			if toLabel == stitch.PublicInternetLabel {
				continue
			}

			if ip := labels[toLabel].IP; ip != "" {
				newHosts[entry{ip, toLabel + ".q"}] = struct{}{}
			}
			for i, cIP := range labels[toLabel].ContainerIPs {
				// The hostname prefix starts from 1 for readability.
				host := fmt.Sprintf("%d.%s.q", i+1, toLabel)
				newHosts[entry{cIP, host}] = struct{}{}
			}
		}
	}

	var hosts []string
	for h := range newHosts {
		hosts = append(hosts, fmt.Sprintf("%-15s %s", h.ip, h.host))
	}

	sort.Strings(hosts)
	return strings.Join(hosts, "\n") + "\n"
}

func namespaceExists(namespace string) (bool, error) {
	nsFullPath := fmt.Sprintf("%s/%s", nsPath, namespace)
	file, err := os.Lstat(nsFullPath)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("error finding file %s: %s", nsFullPath, err)
	}

	if file.Mode()&os.ModeSymlink != os.ModeSymlink {
		return false, nil
	}

	if dst, err := os.Readlink(nsFullPath); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("error finding destination of symlink %s: %s",
			nsFullPath, err)
	} else if dst == fmt.Sprintf("/hostproc/%s/ns/net", namespace) {
		return true, nil
	}
	return false, nil
}

func networkNS(id string) string {
	return fmt.Sprintf("%s_ns", id[0:13])
}

func veths(id string) (in, out string) {
	return fmt.Sprintf("%s_i", id[0:13]), fmt.Sprintf("%s_c", id[0:13])
}

// Generate the temporary internal veth name from the name of the
// external veth
func tempVethPairName(out string) (in string) {
	return fmt.Sprintf("%s_i", out[0:13])
}

func patchPorts(id string) (br, quilt string) {
	return fmt.Sprintf("%s_br", id[0:12]), fmt.Sprintf("%s_q", id[0:12])
}

func ipExec(namespace, format string, args ...interface{}) error {
	_, _, err := ipExecVerbose(namespace, format, args...)
	return err
}

// Use like the `ip` command
//
// For example, if you wanted the stats on `eth0` (as in the command
// `ip link show eth0`) then you would pass in ("", "link show %s", "eth0")
//
// If you wanted to run this in namespace `ns1` then you would use
// ("ns1", "link show %s", "eth0")
//
// Stored in a variable so we can mock it out for the unit tests.
var ipExecVerbose = func(namespace, format string, args ...interface{}) (
	stdout, stderr []byte, err error) {
	cmd := fmt.Sprintf(format, args...)
	cmd = fmt.Sprintf("ip %s", cmd)
	if namespace != "" {
		cmd = fmt.Sprintf("ip netns exec %s %s", namespace, cmd)
	}
	return shVerbose(cmd)
}

func sh(format string, args ...interface{}) error {
	_, _, err := shVerbose(format, args...)
	return err
}

// Returns (Stdout, Stderr, error)
//
// It's critical that the error returned here is the exact error
// from "os/exec" commands
var shVerbose = func(format string, args ...interface{}) (
	stdout, stderr []byte, err error) {
	command := fmt.Sprintf(format, args...)
	cmdArgs := strings.Split(command, " ")
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return nil, nil, err
	}

	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// For debug messages
func namespaceName(namespace string) string {
	if namespace == "" {
		return "root namespace"
	}
	return fmt.Sprintf("%s namespace", namespace)
}

// makeIPRule takes an ip rule as formatted in the output of `iptables -S`,
// and returns the corresponding ipRule. The output options will be in the same
// order as output by `iptables -S`.
func makeIPRule(inputRule string) (ipRule, error) {
	cmdRE := regexp.MustCompile("(-[A-Z]+)\\s+([A-Z]+)")
	cmdMatch := cmdRE.FindSubmatch([]byte(inputRule))
	if len(cmdMatch) < 3 {
		return ipRule{}, fmt.Errorf("missing iptables command")
	}

	var opts string
	optsRE := regexp.MustCompile("-(?:[A-Z]+\\s+)+[A-Z]+\\s+(.*)")
	optsMatch := optsRE.FindSubmatch([]byte(inputRule))

	if len(optsMatch) > 2 {
		return ipRule{}, fmt.Errorf("malformed iptables options")
	}

	if len(optsMatch) == 2 {
		opts = strings.TrimSpace(string(optsMatch[1]))
	}

	rule := ipRule{
		cmd:   strings.TrimSpace(string(cmdMatch[1])),
		chain: strings.TrimSpace(string(cmdMatch[2])),
		opts:  opts,
	}
	return rule, nil
}

func deleteNatRule(rule ipRule) error {
	var command string
	args := fmt.Sprintf("%s %s", rule.chain, rule.opts)
	if rule.cmd == "-A" {
		command = fmt.Sprintf("iptables -t nat -D %s", args)
	} else if rule.cmd == "-N" {
		// Delete new chains.
		command = fmt.Sprintf("iptables -t nat -X %s", rule.chain)
	}

	stdout, _, err := shVerbose(command)
	if err != nil {
		return fmt.Errorf("failed to delete NAT rule %s: %s", command,
			string(stdout))
	}
	return nil
}

func addNatRule(rule ipRule) error {
	args := fmt.Sprintf("%s %s", rule.chain, rule.opts)
	cmd := fmt.Sprintf("iptables -t nat -A %s", args)
	err := sh(cmd)
	if err != nil {
		return fmt.Errorf("failed to add NAT rule %s: %s", cmd, err)
	}
	return nil
}

// getPublicInterface gets the interface with the default route.
func getPublicInterface() (string, error) {
	stdout, _, err := ipExecVerbose("", "route list")
	if err != nil {
		return "", err
	}

	matches := regexp.MustCompile("default .* dev (.*)").FindSubmatch(stdout)
	if len(matches) < 2 {
		return "", errors.New("no default route")
	}

	return strings.TrimSpace(string(matches[1])), nil
}

// The addRoute function adds a new route to the given namespace.
func addRoute(namespace string, r route) error {
	var command string
	if r.isDefault {
		command = fmt.Sprintf("route add default via %s", r.ip)
	} else {
		command = fmt.Sprintf("route add %s via %s", r.ip, r.dev)
	}

	_, _, err := ipExecVerbose(namespace, command)
	if err != nil {
		return fmt.Errorf("failed to add route %s in %s: %s",
			r.ip, namespaceName(namespace), err)
	}
	return nil
}

// deleteRoute adds route to the routing table in namespace.
func deleteRoute(namespace string, r route) error {
	var command string
	if r.isDefault {
		command = fmt.Sprintf("route del default via %s", r.ip)
	} else {
		command = fmt.Sprintf("route delete %s via %s", r.ip, r.dev)
	}

	_, _, err := ipExecVerbose(namespace, command)
	if err != nil {
		return fmt.Errorf("failed to delete route %s in %s: %s",
			r.ip, namespaceName(namespace), err)
	}
	return nil
}

func addOFRules(dk docker.Client, flows []interface{}) error {
	flowCommands := []string{}
	for _, f := range flows {
		flow := f.(OFRule)
		flowCommands = append(flowCommands, fmt.Sprintf("%s,%s,actions=%s",
			flow.table, flow.match, flow.actions))
	}
	flowsString := strings.Join(flowCommands, "\n")

	// XXX: We could skip the intermediary file by using a HEREDOC.
	// add-flows can add all of our flows from a single file at one
	// XXX: Cleanup the temp file.
	flowsTempFile := ".wknet-OFadds"
	err := dk.WriteToContainer(supervisor.Ovsvswitchd, flowsString, "/tmp",
		flowsTempFile, 0644)
	if err != nil {
		return err
	}

	args := fmt.Sprintf("ovs-ofctl add-flows %s %s", quiltBridge,
		"/tmp/"+flowsTempFile)
	err = dk.Exec(supervisor.Ovsvswitchd, strings.Split(args, " ")...)
	if err != nil {
		return err
	}
	return nil
}

func deleteOFRules(dk docker.Client, flows []interface{}) error {
	flowCommands := []string{}
	for _, f := range flows {
		flow := f.(OFRule)
		flowCommands = append(flowCommands, fmt.Sprintf("%s,%s",
			flow.table, flow.match))
	}
	flowsString := strings.Join(flowCommands, " ")
	args := fmt.Sprintf("ovs-ofctl del-flows --strict %s %s",
		quiltBridge, flowsString)
	err := dk.Exec(supervisor.Ovsvswitchd, strings.Split(args, " ")...)
	if err != nil {
		return err
	}
	return nil
}

// makeOFRule constructs an OFRule with the given flow, actions and table.
// table must be of the format table=X, and both flow and action must be
// formatted as in the output from `ovs-ofctl dump-flows` - this includes
// case sensitivity. There must be no spaces in flow and actions.
// This function works for the flows we need at the moment, but it will likely
// have to be extended in the future.
func makeOFRule(flowEntry string) (OFRule, error) {
	tableRE := regexp.MustCompile("table=\\d+")
	if ok := tableRE.MatchString(flowEntry); !ok {
		return OFRule{}, fmt.Errorf("malformed OpenFlow table: %s", flowEntry)
	}
	table := tableRE.FindString(flowEntry)

	fields := strings.Split(flowEntry, " ")
	match := fields[len(fields)-2]
	actions := fields[len(fields)-1]

	actRE := regexp.MustCompile("actions=(\\S+)")
	actMatches := actRE.FindStringSubmatch(actions)
	if len(actMatches) != 2 {
		return OFRule{}, fmt.Errorf("bad OF action format: %s", actions)
	}
	actions = actMatches[1]

	// An action of the format function(args...)
	funcRE := regexp.MustCompile("\\w+\\([^\\)]+\\)")
	funcMatches := funcRE.FindAllString(actions, -1)

	// Remove all functions, so we can split actions on commas
	noFuncs := funcRE.Split(actions, -1)

	var argMatches []string
	for _, a := range noFuncs {
		// XXX: If there are commas between two functions in actions,
		// there might be lone commas in noFuncs. We should ideally
		// get rid of these commas with the regex above.
		act := strings.Trim(string(a), ",")
		if act == "" {
			continue
		}

		splitAct := strings.Split(act, ",")
		argMatches = append(argMatches, splitAct...)
	}

	allMatches := append(argMatches, funcMatches...)
	sort.Strings(allMatches)

	splitMatch := strings.Split(match, ",")
	sort.Strings(splitMatch)

	newRule := OFRule{
		table:   table,
		match:   strings.Join(splitMatch, ","),
		actions: strings.Join(allMatches, ","),
	}
	return newRule, nil
}

func (nsis nsInfoSlice) Get(ii int) interface{} {
	return nsis[ii]
}

func (nsis nsInfoSlice) Len() int {
	return len(nsis)
}

func (nds netdevSlice) Get(ii int) interface{} {
	return nds[ii]
}

func (nds netdevSlice) Len() int {
	return len(nds)
}

func (iprs ipRuleSlice) Get(ii int) interface{} {
	return iprs[ii]
}

func (iprs ipRuleSlice) Len() int {
	return len(iprs)
}

func (rs routeSlice) Get(ii int) interface{} {
	return rs[ii]
}

func (rs routeSlice) Len() int {
	return len(rs)
}

// Get returns the value contained at the given index
func (ofrs OFRuleSlice) Get(ii int) interface{} {
	return ofrs[ii]
}

// Len returns the number of items in the slice
func (ofrs OFRuleSlice) Len() int {
	return len(ofrs)
}
