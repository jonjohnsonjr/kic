package node

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/machine/libmachine/state"
	"github.com/medyagh/kic/pkg/assets"
	"github.com/medyagh/kic/pkg/command"
	"github.com/medyagh/kic/pkg/config/cri"
	"github.com/medyagh/kic/pkg/oci"

	"github.com/pkg/errors"
)

const (
	// Docker default bridge network is named "bridge" (https://docs.docker.com/network/bridge/#use-the-default-bridge-network)
	DefaultNetwork  = "bridge"
	ClusterLabelKey = "io.k8s.sigs.kic.cluster" // ClusterLabelKey is applied to each node docker container for identification
	NodeRoleKey     = "io.k8s.sigs.kic.role"
	DefaultOci      = "docker"
)

// Node represents a handle to a kic node
// This struct must be created by one of: CreateControlPlane
type Node struct {
	// must be one of docker container ID or name
	name string
	// cached node info etc.
	cache *nodeCache
	R     command.Runner // Runner
}

// WriteFile writes content to dest on the node
func (n *Node) WriteFile(dest, content string, perm string) error {
	// create destination directory
	cmd := exec.Command("mkdir", "-p", filepath.Dir(dest))
	rr, err := n.R.RunCmd(cmd)
	if err != nil {
		return errors.Wrapf(err, "failed to create directory %s cmd: %v output:%q", cmd.Args, dest, rr.Output())
	}

	cmd = exec.Command("cp", "/dev/stdin", dest)
	cmd.Stdin = strings.NewReader(content)

	if rr, err := n.R.RunCmd(cmd); err != nil {
		return errors.Wrapf(err, "failed to run: cp /dev/stdin %s cmd: %v output:%q", dest, cmd.Args, rr.Output())
	}

	cmd = exec.Command("chmod", perm, dest)
	_, err = n.R.RunCmd(cmd)
	if err != nil {
		return errors.Wrapf(err, "failed to run: chmod %s %s", perm, dest)
	}
	return nil
}

// IP returns the IP address of the node
func (n *Node) IP() (ipv4 string, ipv6 string, err error) {
	// use the cached version first
	cachedIPv4, cachedIPv6 := n.cache.IP()
	if cachedIPv4 != "" && cachedIPv6 != "" {
		return cachedIPv4, cachedIPv6, nil
	}
	// retrieve the IP address of the node using docker inspect
	lines, err := oci.Inspect(n.name, "{{range .NetworkSettings.Networks}}{{.IPAddress}},{{.GlobalIPv6Address}}{{end}}")
	if err != nil {
		return "", "", errors.Wrap(err, "failed to get container details")
	}
	if len(lines) != 1 {
		return "", "", errors.Errorf("file should only be one line, got %d lines", len(lines))
	}
	ips := strings.Split(lines[0], ",")
	if len(ips) != 2 {
		return "", "", errors.Errorf("container addresses should have 2 values, got %d values", len(ips))
	}
	n.cache.set(func(cache *nodeCache) {
		cache.ipv4 = ips[0]
		cache.ipv6 = ips[1]
	})
	return ips[0], ips[1], nil
}

// LoadImageArchive loads an image from archive into the node
func (n *Node) LoadImageArchive(image io.Reader) error {
	cmd := exec.Command(
		"ctr", "--namespace=k8s.io", "images", "import", "-",
	)
	cmd.Stdin = image
	if _, err := n.R.RunCmd(cmd); err != nil {
		return errors.Wrap(err, "failed to load image")
	}
	return nil
}

// Copy copies a local asset into the node
func (n *Node) Copy(ociBinary string, asset assets.CopyAsset) error {
	if err := oci.Copy(ociBinary, n.name, asset); err != nil {
		return errors.Wrap(err, "failed to copy file/folder")
	}

	cmd := exec.Command("chmod", asset.Permissions, asset.TargetPath())
	if _, err := n.R.RunCmd(cmd); err != nil {
		return errors.Wrap(err, "failed to chmod file permissions")
	}
	return nil
}

// Status gets status for node
func (n *Node) Status() (state.State, error) {
	return oci.Status(DefaultOci, n.name)
}

// Pause pauses all process in the node
func (n *Node) Pause() error {
	return oci.Pause(DefaultOci, n.name)
}

// Stop stops the node
func (n *Node) Stop() error {
	return oci.Stop(n.name)
}

// Remove removes the node
func (n *Node) Remove() error {
	return oci.Remove(DefaultOci, n.name)
}

type CreateParams struct {
	Name         string // used for container name and hostname
	Image        string // container image to use to create the node.
	ClusterLabel string
	Role         string // currently only role supported is control-plane
	Mounts       []cri.Mount
	PortMappings []cri.PortMapping
	Cpus         string
	Memory       string
	Envs         map[string]string
	ExtraArgs    []string
}

// todo use a struct for this
func CreateNode(p CreateParams, cmder command.Runner) (*Node, error) {
	runArgs := []string{
		fmt.Sprintf("--cpus=%s", p.Cpus),
		fmt.Sprintf("--memory=%s", p.Memory),
		"-d", // run the container detached
		"-t", // allocate a tty for entrypoint logs
		// running containers in a container requires privileged
		// NOTE: we could try to replicate this with --cap-add, and use less
		// privileges, but this flag also changes some mounts that are necessary
		// including some ones docker would otherwise do by default.
		// for now this is what we want. in the future we may revisit this.
		"--privileged",
		"--security-opt", "seccomp=unconfined", // also ignore seccomp
		"--tmpfs", "/tmp", // various things depend on working /tmp
		"--tmpfs", "/run", // systemd wants a writable /run
		// some k8s things want /lib/modules
		"-v", "/lib/modules:/lib/modules:ro",
		"--hostname", p.Name, // make hostname match container name
		"--name", p.Name, // ... and set the container name
		// label the node with the cluster ID
		"--label", p.ClusterLabel,
		// label the node with the role ID
		"--label", fmt.Sprintf("%s=%s", NodeRoleKey, p.Role),
	}

	for key, val := range p.Envs {
		runArgs = append(runArgs, "-e", fmt.Sprintf("%s=%s", key, val))
	}

	// adds node specific args
	runArgs = append(runArgs, p.ExtraArgs...)

	if oci.UsernsRemap() {
		// We need this argument in order to make this command work
		// in systems that have userns-remap enabled on the docker daemon
		runArgs = append(runArgs, "--userns=host")
	}

	_, err := oci.CreateContainer(DefaultOci,
		p.Image,
		oci.WithRunArgs(runArgs...),
		oci.WithMounts(p.Mounts),
		oci.WithPortMappings(p.PortMappings),
	)

	if err != nil {
		return nil, errors.Wrap(err, "oci create ")
	}

	// we should return a handle so the caller can clean it up
	node, err := Find(p.Name, cmder)
	if err != nil {
		return node, errors.Wrap(err, "find node")
	}

	return node, nil
}

// Find finds a node
func Find(name string, cmder command.Runner) (*Node, error) {
	_, err := oci.Inspect(name, "{{.Id}}")
	if err != nil {
		return nil, fmt.Errorf("can't find node %v", err)
	}
	return &Node{
		name:  name,
		cache: &nodeCache{},
		R:     cmder,
	}, nil
}
