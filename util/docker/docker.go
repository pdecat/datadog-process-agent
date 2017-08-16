package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/gopsutil/process"
	log "github.com/cihub/seelog"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"github.com/DataDog/datadog-process-agent/util"
)

var (
	ErrDockerNotAvailable = errors.New("docker not available")

	globalDockerUtil     *dockerUtil
	invalidationInterval = 5 * time.Minute
	lastErr              string

	// NullContainer is an empty container object that has
	// default values for all fields including sub-fields.
	// If new sub-structs are added to Container this must
	// be updated.
	NullContainer = &Container{
		CPU:     &CgroupTimesStat{},
		Memory:  &CgroupMemStat{},
		IO:      &CgroupIOStat{},
		Network: &NetworkStat{},
	}
)

type NetworkStat struct {
	BytesSent   uint64
	BytesRcvd   uint64
	PacketsSent uint64
	PacketsRcvd uint64
}

type containerFilter struct {
	Enabled        bool
	ImageWhitelist []*regexp.Regexp
	NameWhitelist  []*regexp.Regexp
	ImageBlacklist []*regexp.Regexp
	NameBlacklist  []*regexp.Regexp
}

// NewcontainerFilter creates a new container filter from a two slices of
// regexp patterns for a whitelist and blacklist. Each pattern should have
// the following format: "field:pattern" where field can be: [image, name].
// An error is returned if any of the expression don't compile.
func newContainerFilter(whitelist, blacklist []string) (*containerFilter, error) {
	iwl, nwl, err := parseFilters(whitelist)
	if err != nil {
		return nil, err
	}
	ibl, nbl, err := parseFilters(blacklist)
	if err != nil {
		return nil, err
	}

	return &containerFilter{
		Enabled:        len(whitelist) > 0 || len(blacklist) > 0,
		ImageWhitelist: iwl,
		NameWhitelist:  nwl,
		ImageBlacklist: ibl,
		NameBlacklist:  nbl,
	}, nil
}

func parseFilters(filters []string) (imageFilters, nameFilters []*regexp.Regexp, err error) {
	for _, filter := range filters {
		switch {
		case strings.HasPrefix(filter, "image:"):
			pat := strings.TrimPrefix(filter, "image:")
			r, err := regexp.Compile(strings.TrimPrefix(pat, "image:"))
			if err != nil {
				return nil, nil, fmt.Errorf("invalid regex '%s': %s", pat, err)
			}
			imageFilters = append(imageFilters, r)
		case strings.HasPrefix(filter, "name:"):
			pat := strings.TrimPrefix(filter, "name:")
			r, err := regexp.Compile(pat)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid regex '%s': %s", pat, err)
			}
			nameFilters = append(nameFilters, r)
		}
	}
	return imageFilters, nameFilters, nil
}

// IsExcluded returns a bool indicating if the container should be excluded
// based on the filters in the containerFilter instance.
func (cf containerFilter) IsExcluded(container *Container) bool {
	if !cf.Enabled {
		return false
	}

	var excluded bool
	for _, r := range cf.ImageBlacklist {
		if r.MatchString(container.Image) {
			excluded = true
			break
		}
	}
	for _, r := range cf.NameBlacklist {
		if r.MatchString(container.Name) {
			excluded = true
			break
		}
	}

	// Any excluded container could be whitelisted.
	if excluded {
		for _, r := range cf.ImageWhitelist {
			if r.MatchString(container.Image) {
				return false
			}
		}
		for _, r := range cf.NameWhitelist {
			if r.MatchString(container.Name) {
				return false
			}
		}
	}
	return excluded
}

type Container struct {
	Type    string
	ID      string
	Name    string
	Image   string
	ImageID string
	Created int64
	State   string
	Health  string
	Pids    []int32

	CPU     *CgroupTimesStat
	Memory  *CgroupMemStat
	IO      *CgroupIOStat
	Network *NetworkStat
}

type dockerNetwork struct {
	iface      string
	dockerName string
}

type dockerNetworks []dockerNetwork

func (a dockerNetworks) Len() int           { return len(a) }
func (a dockerNetworks) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a dockerNetworks) Less(i, j int) bool { return a[i].dockerName < a[j].dockerName }

// Config is an exported configuration object that is used when
// initializing the DockerUtil.
type Config struct {
	// CollectHealth enables health collection. This requires calling a
	// container.Inspect for each container on every called to getContainers().
	CollectHealth bool
	// CollectNetwork enables network stats collection. This requires at least
	// one call to container.Inspect for new containers and reads from the
	// procfs for stats.
	CollectNetwork bool
	// Whitelist is a slice of filter strings in the form of key:regex where key
	// is either 'image' or 'name' and regex is a valid regular expression.
	Whitelist []string
	// Blacklist is the same as whitelist but for exclusion.
	Blacklist []string

	// internal use only
	filter *containerFilter
}

// dockerUtil wraps interactions with a local docker API.
type dockerUtil struct {
	cfg *Config
	cli *client.Client
	// tracks the last time we invalidate our internal caches
	lastInvalidate time.Time
	// networkMappings by container id
	networkMappings map[string][]dockerNetwork
	// image sha mapping cache
	imageNameBySha map[string]string
	sync.Mutex
}

//
// Expose module-level functions that will interact with a Singleton dockerUtil.

// ContainersForPids gets a mapping of pids -> container for the given PIDs.
func ContainersForPIDs(pids []int32) map[int32]*Container {
	if globalDockerUtil != nil {
		r, err := globalDockerUtil.containersForPIDs(pids)
		if err != nil && err.Error() != lastErr {
			log.Warnf("unable to collect docker stats: %s", err)
			lastErr = err.Error()
		} else {
			return r
		}
	}
	return map[int32]*Container{}
}

// AllContainers returns a slice of all running containers.
func AllContainers() ([]*Container, error) {
	pids, err := process.Pids()
	if err != nil {
		return nil, fmt.Errorf("could not get pids: %s", err)
	}

	if globalDockerUtil != nil {
		r, err := globalDockerUtil.containers(pids)
		if err != nil && err.Error() != lastErr {
			log.Warnf("unable to collect docker stats: %s", err)
			lastErr = err.Error()
		} else {
			return r, nil
		}
	}
	return nil, nil
}

func GetHostname() (string, error) {
	if globalDockerUtil != nil {
		return "", ErrDockerNotAvailable
	}
	return globalDockerUtil.getHostname()
}

// IsContainerized returns True if we're running in the docker-dd-agent container.
func IsContainerized() bool {
	return os.Getenv("DOCKER_DD_AGENT") == "yes"
}

// InitDockerUtil initializes the global dockerUtil singleton. This _must_ be
// called before accessing any of the top-level docker calls.
func InitDockerUtil(cfg *Config) error {
	// If we don't have a docker.sock then return a known error.
	sockPath := util.GetEnv("DOCKER_SOCKET_PATH", "/var/run/docker.sock")
	if !util.PathExists(sockPath) {
		return ErrDockerNotAvailable
	}
	// The /proc/mounts file won't be availble on non-Linux systems.
	mountsFile := "/proc/mounts"
	if !util.PathExists(mountsFile) {
		return ErrDockerNotAvailable
	}

	serverVersion, err := detectServerAPIVersion()
	if err != nil {
		return err
	}
	os.Setenv("DOCKER_API_VERSION", serverVersion)

	// Connect again using the known server version.
	cli, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	// Pre-parse the filter and use that internally.
	cfg.filter, err = newContainerFilter(cfg.Whitelist, cfg.Blacklist)
	if err != nil {
		return err
	}

	globalDockerUtil = &dockerUtil{
		cfg:             cfg,
		cli:             cli,
		networkMappings: make(map[string][]dockerNetwork),
		imageNameBySha:  make(map[string]string),
		lastInvalidate:  time.Now(),
	}
	return nil
}

// getContainers returns a list of Docker info for active containers using the
// Docker API. This requires the running user to be in the "docker" user group
// or have access to /tmp/docker.sock.
func (d *dockerUtil) getContainers() ([]*Container, error) {
	containers, err := d.cli.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	ret := make([]*Container, 0, len(containers))
	for _, c := range containers {
		var health string
		var i types.ContainerJSON
		if d.cfg.CollectHealth {
			// We could have lost the container between list and inspect so ignore these errors.
			i, err = d.cli.ContainerInspect(context.Background(), c.ID)
			if err != nil && client.IsErrContainerNotFound(err) {
				return nil, err
			}
			// Healthcheck and status not available until >= 1.12
			if i.State.Health != nil {
				health = i.State.Health.Status
			}
		}

		if d.cfg.CollectNetwork {
			// FIXME: We might need to invalidate this cache if a containers networks are changed live.
			d.Lock()
			if _, ok := d.networkMappings[c.ID]; !ok {
				if i.ContainerJSONBase == nil {
					i, err = d.cli.ContainerInspect(context.Background(), c.ID)
					if err != nil && client.IsErrContainerNotFound(err) {
						d.Unlock()
						return nil, err
					}
				}
				d.networkMappings[c.ID] = findDockerNetworks(c.ID, i.State.Pid, c.NetworkSettings)
			}
			d.Unlock()
		}

		container := &Container{
			Type:    "Docker",
			ID:      c.ID,
			Name:    c.Names[0],
			Image:   d.extractImageName(c.Image),
			ImageID: c.ImageID,
			Created: c.Created,
			State:   c.State,
			Health:  health,
		}
		if !d.cfg.filter.IsExcluded(container) {
			ret = append(ret, container)
		}
	}

	if d.lastInvalidate.Add(invalidationInterval).After(time.Now()) {
		d.invalidateCaches(containers)
	}

	return ret, nil
}

// containers gets a list of all containers on the current node using a mix of
// the Docker APIs and cgroups stats. We attempt to limit syscalls where possible.
func (d *dockerUtil) containers(pids []int32) ([]*Container, error) {
	cgByContainer, err := CgroupsForPids(pids)
	if err != nil {
		return nil, err
	}
	containers, err := d.getContainers()
	if err != nil {
		return nil, err
	}
	for _, container := range containers {
		cgroup, ok := cgByContainer[container.ID]
		if !ok {
			continue
		}
		container.Memory, err = cgroup.Mem()
		if err != nil {
			return nil, err
		}
		container.CPU, err = cgroup.CPU()
		if err != nil {
			return nil, err
		}
		container.IO, err = cgroup.IO()
		if err != nil {
			return nil, err
		}

		if d.cfg.CollectNetwork {
			d.Lock()
			networks, ok := d.networkMappings[cgroup.ContainerID]
			d.Unlock()
			if ok && len(cgroup.Pids) > 0 {
				netStat, err := collectNetworkStats(cgroup.ContainerID, int(cgroup.Pids[0]), networks)
				if err != nil {
					return nil, err
				}
				container.Network = netStat
			}
		} else {
			container.Network = NullContainer.Network
		}

		container.Pids = cgroup.Pids
	}
	return containers, nil
}

// containersForPIDs generates a mapping of PIDs to container using containers()
func (d *dockerUtil) containersForPIDs(pids []int32) (map[int32]*Container, error) {
	containers, err := d.containers(pids)
	if err != nil {
		return nil, err
	}
	containerMap := make(map[int32]*Container)
	for _, container := range containers {
		for _, p := range container.Pids {
			containerMap[p] = container
		}
	}
	return containerMap, nil
}

func (d *dockerUtil) getHostname() (string, error) {
	info, err := d.cli.Info(context.Background())
	if err != nil {
		return "", fmt.Errorf("unable to get Docker info: %s", err)
	}
	return info.Name, nil
}

// extractImageName will resolve sha image name to their user-friendly name.
// For non-sha names we will just return the name as-is.
func (d *dockerUtil) extractImageName(image string) string {
	if !strings.HasPrefix(image, "sha256:") {
		return image
	}

	d.Lock()
	defer d.Unlock()
	if _, ok := d.imageNameBySha[image]; !ok {
		r, _, err := d.cli.ImageInspectWithRaw(context.Background(), image)
		if err != nil {
			// Only log errors that aren't "not found" because some images may
			// just not be available in docker inspect.
			if !client.IsErrNotFound(err) {
				log.Errorf("could not extract image %s name: %s", err)
			}
			d.imageNameBySha[image] = image
		}

		// Try RepoTags first and fall back to RepoDigest otherwise.
		if len(r.RepoTags) > 0 {
			d.imageNameBySha[image] = r.RepoTags[0]
		} else if len(r.RepoDigests) > 0 {
			// Digests formatted like quay.io/foo/bar@sha256:hash
			sp := strings.SplitN(r.RepoDigests[0], "@", 2)
			d.imageNameBySha[image] = sp[0]
		} else {
			d.imageNameBySha[image] = image
		}
	}
	return d.imageNameBySha[image]
}

func (d *dockerUtil) invalidateCaches(containers []types.Container) {
	liveContainers := make(map[string]struct{})
	liveImages := make(map[string]struct{})
	for _, c := range containers {
		liveContainers[c.ID] = struct{}{}
		liveImages[c.Image] = struct{}{}
	}
	d.Lock()
	for cid := range d.networkMappings {
		if _, ok := liveContainers[cid]; !ok {
			delete(d.networkMappings, cid)
		}
	}
	for image := range d.imageNameBySha {
		if _, ok := liveImages[image]; !ok {
			delete(d.imageNameBySha, image)
		}
	}
	d.Unlock()
}

func detectServerAPIVersion() (string, error) {
	if os.Getenv("DOCKER_API_VERSION") != "" {
		return os.Getenv("DOCKER_API_VERSION"), nil
	}
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = client.DefaultDockerHost
	}
	cli, err := client.NewClient(host, "", nil, nil)
	if err != nil {
		return "", err
	}

	// Create the client using the server's API version
	v, err := cli.ServerVersion(context.Background())
	if err != nil {
		return "", err
	}
	return v.APIVersion, nil
}

var hostNetwork = dockerNetwork{"eth0", "bridge"}

func findDockerNetworks(containerID string, pid int, netSettings *types.SummaryNetworkSettings) []dockerNetwork {
	var err error
	dockerGateways := make(map[string]int64)
	for netName, netConf := range netSettings.Networks {
		gw := netConf.Gateway
		if netName == "host" || gw == "" {
			log.Debugf("Empty network gateway, container %s is in network host mode, its network metrics are for the whole host", containerID)
			return []dockerNetwork{hostNetwork}
		}

		// Check if this is a CIDR or just an IP
		var ip net.IP
		if strings.Contains(gw, "/") {
			ip, _, err = net.ParseCIDR(gw)
			if err != nil {
				log.Warnf("Invalid gateway %s for container id %s: %s, skipping", gw, containerID, err)
				continue
			}
		} else {
			ip = net.ParseIP(gw)
			if ip == nil {
				log.Warnf("Invalid gateway %s for container id %s: %s, skipping", gw, containerID, err)
				continue
			}
		}

		// Convert IP to int64 for comparison to network routes.
		dockerGateways[netName] = int64(binary.BigEndian.Uint32(ip.To4()))
	}

	// Read contents of file. Handle missing or unreadable file in case container was stopped.
	procNetFile := util.HostProc(strconv.Itoa(int(pid)), "net", "route")
	if !util.PathExists(procNetFile) {
		log.Debugf("Missing %s for container %s", procNetFile, containerID)
		return nil
	}
	lines, err := util.ReadLines(procNetFile)
	if err != nil {
		log.Debugf("Unable to read %s for container %s", procNetFile, containerID)
		return nil
	}
	if len(lines) < 1 {
		log.Errorf("empty network file, unable to get docker networks: %s", procNetFile)
		return nil
	}

	networks := make([]dockerNetwork, 0)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if fields[0] == "00000000" {
			continue
		}
		dest, _ := strconv.ParseInt(fields[1], 16, 32)
		mask, _ := strconv.ParseInt(fields[7], 16, 32)
		for net, gw := range dockerGateways {
			if gw&mask == dest {
				networks = append(networks, dockerNetwork{fields[0], net})
			}
		}
	}
	sort.Sort(dockerNetworks(networks))
	return networks
}

func collectNetworkStats(containerID string, pid int, networks []dockerNetwork) (*NetworkStat, error) {
	procNetFile := util.HostProc(strconv.Itoa(int(pid)), "net", "dev")
	if !util.PathExists(procNetFile) {
		log.Debugf("Unable to read %s for container %s", procNetFile, containerID)
		return &NetworkStat{}, nil
	}
	lines, err := util.ReadLines(procNetFile)
	if err != nil {
		log.Debugf("Unable to read %s for container %s", procNetFile, containerID)
		return &NetworkStat{}, nil
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("invalid format for %s", procNetFile)
	}

	nwByIface := make(map[string]dockerNetwork)
	for _, nw := range networks {
		nwByIface[nw.iface] = nw
	}

	// Format:
	//
	// Inter-|   Receive                                                |  Transmit
	// face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
	// eth0:    1296      16    0    0    0     0          0         0        0       0    0    0    0     0       0          0
	// lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
	//
	stat := &NetworkStat{}
	for _, line := range lines[2:] {
		fields := strings.Fields(line)
		if len(fields) < 11 {
			continue
		}
		iface := fields[0][:len(fields[0])-1]

		if _, ok := nwByIface[iface]; ok {
			rcvd, _ := strconv.Atoi(fields[1])
			stat.BytesRcvd += uint64(rcvd)
			pktRcvd, _ := strconv.Atoi(fields[2])
			stat.PacketsRcvd += uint64(pktRcvd)
			sent, _ := strconv.Atoi(fields[9])
			stat.BytesSent += uint64(sent)
			pktSent, _ := strconv.Atoi(fields[10])
			stat.PacketsSent += uint64(pktSent)
		}
	}
	return stat, nil
}
