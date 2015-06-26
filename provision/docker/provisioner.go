// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	clusterLog "github.com/tsuru/docker-cluster/log"
	"github.com/tsuru/tsuru/api/shutdown"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/cmd"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/router"
	_ "github.com/tsuru/tsuru/router/galeb"
	_ "github.com/tsuru/tsuru/router/hipache"
	_ "github.com/tsuru/tsuru/router/routertest"
	_ "github.com/tsuru/tsuru/router/vulcand"
	"gopkg.in/mgo.v2/bson"
)

var mainDockerProvisioner *dockerProvisioner

func init() {
	mainDockerProvisioner = &dockerProvisioner{}
	provision.Register("docker", mainDockerProvisioner)
}

func getRouterForApp(app provision.App) (router.Router, error) {
	routerName, err := app.GetRouter()
	if err != nil {
		return nil, err
	}
	return router.Get(routerName)
}

type dockerProvisioner struct {
	cluster        *cluster.Cluster
	collectionName string
	storage        cluster.Storage
	scheduler      *segregatedScheduler
	isDryMode      bool
}

func (p *dockerProvisioner) initDockerCluster() error {
	debug, _ := config.GetBool("debug")
	clusterLog.SetDebug(debug)
	clusterLog.SetLogger(log.GetStdLogger())
	var err error
	p.storage, err = buildClusterStorage()
	if err != nil {
		return err
	}
	if p.collectionName == "" {
		name, err := config.GetString("docker:collection")
		if err != nil {
			return err
		}
		p.collectionName = name
	}
	var nodes []cluster.Node
	totalMemoryMetadata, _ := config.GetString("docker:scheduler:total-memory-metadata")
	maxUsedMemory, _ := config.GetFloat("docker:scheduler:max-used-memory")
	p.scheduler = &segregatedScheduler{
		maxMemoryRatio:      float32(maxUsedMemory),
		totalMemoryMetadata: totalMemoryMetadata,
		provisioner:         p,
	}
	p.cluster, err = cluster.New(p.scheduler, p.storage, nodes...)
	if err != nil {
		return err
	}
	autoHealingNodes, _ := config.GetBool("docker:healing:heal-nodes")
	if autoHealingNodes {
		disabledSeconds, _ := config.GetDuration("docker:healing:disabled-time")
		if disabledSeconds <= 0 {
			disabledSeconds = 30
		}
		maxFailures, _ := config.GetInt("docker:healing:max-failures")
		if maxFailures <= 0 {
			maxFailures = 5
		}
		waitSecondsNewMachine, _ := config.GetDuration("docker:healing:wait-new-time")
		if waitSecondsNewMachine <= 0 {
			waitSecondsNewMachine = 5 * 60
		}
		healer := nodeHealer{
			locks:                 make(map[string]*sync.Mutex),
			provisioner:           p,
			disabledTime:          disabledSeconds * time.Second,
			waitTimeNewMachine:    waitSecondsNewMachine * time.Second,
			failuresBeforeHealing: maxFailures,
		}
		shutdown.Register(&healer)
		p.cluster.Healer = &healer
	}
	healContainersSeconds, _ := config.GetDuration("docker:healing:heal-containers-timeout")
	if healContainersSeconds > 0 {
		contHealerInst := containerHealer{
			provisioner:         p,
			maxUnresponsiveTime: healContainersSeconds * time.Second,
			done:                make(chan bool),
		}
		shutdown.Register(&contHealerInst)
		go contHealerInst.runContainerHealer()
	}
	activeMonitoring, _ := config.GetDuration("docker:healing:active-monitoring-interval")
	if activeMonitoring > 0 {
		p.cluster.StartActiveMonitoring(activeMonitoring * time.Second)
	}
	autoScaleEnabled, _ := config.GetBool("docker:auto-scale:enabled")
	if autoScaleEnabled {
		autoScale := p.initAutoScaleConfig()
		shutdown.Register(autoScale)
		go autoScale.run()
	}
	return nil
}

func (p *dockerProvisioner) initAutoScaleConfig() *autoScaleConfig {
	waitSecondsNewMachine, _ := config.GetDuration("docker:auto-scale:wait-new-time")
	groupByMetadata, _ := config.GetString("docker:auto-scale:group-by-metadata")
	matadataFilter, _ := config.GetString("docker:auto-scale:metadata-filter")
	maxContainerCount, _ := config.GetInt("docker:auto-scale:max-container-count")
	runInterval, _ := config.GetDuration("docker:auto-scale:run-interval")
	scaleDownRatio, _ := config.GetFloat("docker:auto-scale:scale-down-ratio")
	preventRebalance, _ := config.GetBool("docker:auto-scale:prevent-rebalance")
	totalMemoryMetadata, _ := config.GetString("docker:scheduler:total-memory-metadata")
	maxUsedMemory, _ := config.GetFloat("docker:scheduler:max-used-memory")
	return &autoScaleConfig{
		provisioner:         p,
		groupByMetadata:     groupByMetadata,
		totalMemoryMetadata: totalMemoryMetadata,
		maxMemoryRatio:      float32(maxUsedMemory),
		maxContainerCount:   maxContainerCount,
		matadataFilter:      matadataFilter,
		scaleDownRatio:      float32(scaleDownRatio),
		waitTimeNewMachine:  waitSecondsNewMachine * time.Second,
		runInterval:         runInterval * time.Second,
		preventRebalance:    preventRebalance,
		done:                make(chan bool),
	}
}

func (p *dockerProvisioner) cloneProvisioner(ignoredContainers []container) (*dockerProvisioner, error) {
	var err error
	overridenProvisioner := *p
	containerIds := make([]string, len(ignoredContainers))
	for i := range ignoredContainers {
		containerIds[i] = ignoredContainers[i].ID
	}
	overridenProvisioner.scheduler = &segregatedScheduler{
		maxMemoryRatio:      p.scheduler.maxMemoryRatio,
		totalMemoryMetadata: p.scheduler.totalMemoryMetadata,
		provisioner:         &overridenProvisioner,
		ignoredContainers:   containerIds,
	}
	overridenProvisioner.cluster, err = cluster.New(overridenProvisioner.scheduler, p.storage)
	if err != nil {
		return nil, err
	}
	overridenProvisioner.cluster.Healer = p.cluster.Healer
	return &overridenProvisioner, nil
}

func (p *dockerProvisioner) stopDryMode() {
	if p.isDryMode {
		p.cluster.StopDryMode()
		p.collection().DropCollection()
	}
}

func (p *dockerProvisioner) dryMode(ignoredContainers []container) (*dockerProvisioner, error) {
	var err error
	overridenProvisioner := &dockerProvisioner{
		collectionName: "containers_dry_" + randomString(),
		isDryMode:      true,
	}
	containerIds := make([]string, len(ignoredContainers))
	for i := range ignoredContainers {
		containerIds[i] = ignoredContainers[i].ID
	}
	overridenProvisioner.scheduler = &segregatedScheduler{
		maxMemoryRatio:      p.scheduler.maxMemoryRatio,
		totalMemoryMetadata: p.scheduler.totalMemoryMetadata,
		provisioner:         overridenProvisioner,
		ignoredContainers:   containerIds,
	}
	overridenProvisioner.cluster, err = cluster.New(overridenProvisioner.scheduler, p.storage)
	if err != nil {
		return nil, err
	}
	overridenProvisioner.cluster.DryMode()
	containersToCopy, err := p.listAllContainers()
	if err != nil {
		return nil, err
	}
	coll := overridenProvisioner.collection()
	defer coll.Close()
	toInsert := make([]interface{}, len(containersToCopy))
	for i := range containersToCopy {
		toInsert[i] = containersToCopy[i]
	}
	if len(toInsert) > 0 {
		err = coll.Insert(toInsert...)
		if err != nil {
			return nil, err
		}
	}
	return overridenProvisioner, nil
}

func (p *dockerProvisioner) getCluster() *cluster.Cluster {
	if p.cluster == nil {
		panic("nil cluster")
	}
	return p.cluster
}

func (p *dockerProvisioner) StartupMessage() (string, error) {
	nodeList, err := p.getCluster().UnfilteredNodes()
	if err != nil {
		return "", err
	}
	out := "Docker provisioner reports the following nodes:\n"
	for _, node := range nodeList {
		out += fmt.Sprintf("    Docker node: %s\n", node.Address)
	}
	return out, nil
}

func (p *dockerProvisioner) Initialize() error {
	return p.initDockerCluster()
}

// Provision creates a route for the container
func (p *dockerProvisioner) Provision(app provision.App) error {
	r, err := getRouterForApp(app)
	if err != nil {
		log.Fatalf("Failed to get router: %s", err)
		return err
	}
	return r.AddBackend(app.GetName())
}

func (p *dockerProvisioner) Restart(a provision.App, w io.Writer) error {
	containers, err := p.listContainersByApp(a.GetName())
	if err != nil {
		return err
	}
	imageId, err := appCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	if w == nil {
		w = ioutil.Discard
	}
	writer := &app.LogWriter{App: a, Writer: w}
	_, err = p.runReplaceUnitsPipeline(writer, a, containers, imageId)
	return err
}

func (p *dockerProvisioner) Start(app provision.App) error {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		return errors.New(fmt.Sprintf("Got error while getting app containers: %s", err))
	}
	return runInContainers(containers, func(c *container, _ chan *container) error {
		err := c.start(p, false)
		if err != nil {
			return err
		}
		c.setStatus(p, provision.StatusStarting.String())
		if info, err := c.networkInfo(p); err == nil {
			p.fixContainer(c, info)
		}
		return nil
	}, nil, true)
}

func (p *dockerProvisioner) Stop(app provision.App) error {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		log.Errorf("Got error while getting app containers: %s", err)
		return nil
	}
	return runInContainers(containers, func(c *container, _ chan *container) error {
		err := c.stop(p)
		if err != nil {
			log.Errorf("Failed to stop %q: %s", app.GetName(), err)
		}
		return err
	}, nil, true)
}

func (p *dockerProvisioner) Swap(app1, app2 provision.App) error {
	r, err := getRouterForApp(app1)
	if err != nil {
		return err
	}
	return r.Swap(app1.GetName(), app2.GetName())
}

func (p *dockerProvisioner) ImageDeploy(app provision.App, imageId string, w io.Writer) (string, error) {
	isValid, err := isValidAppImage(app.GetName(), imageId)
	if err != nil {
		return "", err
	}
	if !isValid {
		return "", fmt.Errorf("invalid image for app %s: %s", app.GetName(), imageId)
	}
	return imageId, p.deploy(app, imageId, w)
}

func (p *dockerProvisioner) GitDeploy(app provision.App, version string, w io.Writer) (string, error) {
	imageId, err := p.gitDeploy(app, version, w)
	if err != nil {
		return "", err
	}
	return imageId, p.deployAndClean(app, imageId, w)
}

func (p *dockerProvisioner) ArchiveDeploy(app provision.App, archiveURL string, w io.Writer) (string, error) {
	imageId, err := p.archiveDeploy(app, p.getBuildImage(app), archiveURL, w)
	if err != nil {
		return "", err
	}
	return imageId, p.deployAndClean(app, imageId, w)
}

func (p *dockerProvisioner) UploadDeploy(app provision.App, archiveFile io.ReadCloser, w io.Writer) (string, error) {
	defer archiveFile.Close()
	filePath := "/home/application/archive.tar.gz"
	user, err := config.GetString("docker:user")
	if err != nil {
		user, _ = config.GetString("docker:ssh:user")
	}
	options := docker.CreateContainerOptions{
		Config: &docker.Config{
			AttachStdout: true,
			AttachStderr: true,
			AttachStdin:  true,
			OpenStdin:    true,
			StdinOnce:    true,
			User:         user,
			Image:        p.getBuildImage(app),
			Cmd:          []string{"/bin/bash", "-c", "cat > " + filePath},
		},
	}
	cluster := p.getCluster()
	_, container, err := cluster.CreateContainerSchedulerOpts(options, app.GetName())
	if err != nil {
		return "", err
	}
	defer cluster.RemoveContainer(docker.RemoveContainerOptions{ID: container.ID, Force: true})
	err = cluster.StartContainer(container.ID, nil)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	err = cluster.AttachToContainer(docker.AttachToContainerOptions{
		Container:    container.ID,
		OutputStream: &output,
		ErrorStream:  &output,
		InputStream:  archiveFile,
		Stream:       true,
		Stdin:        true,
		Stdout:       true,
		Stderr:       true,
	})
	if err != nil {
		return "", err
	}
	status, err := cluster.WaitContainer(container.ID)
	if err != nil {
		return "", err
	}
	if status != 0 {
		log.Errorf("Failed to deploy container from upload: %s", &output)
		return "", fmt.Errorf("container exited with status %d", status)
	}
	image, err := cluster.CommitContainer(docker.CommitContainerOptions{Container: container.ID})
	if err != nil {
		return "", err
	}
	imageId, err := p.archiveDeploy(app, image.ID, "file://"+filePath, w)
	if err != nil {
		return "", err
	}
	return imageId, p.deployAndClean(app, imageId, w)
}

func (p *dockerProvisioner) deployAndClean(a provision.App, imageId string, w io.Writer) error {
	err := p.deploy(a, imageId, w)
	if err != nil {
		p.cleanImage(a.GetName(), imageId)
	}
	return err
}

func (p *dockerProvisioner) deploy(a provision.App, imageId string, w io.Writer) error {
	containers, err := p.listContainersByApp(a.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		_, err = p.runCreateUnitsPipeline(w, a, 1, imageId)
	} else {
		_, err = p.runReplaceUnitsPipeline(w, a, containers, imageId)
	}
	return err
}

func (p *dockerProvisioner) Destroy(app provision.App) error {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		log.Errorf("Failed to list app containers: %s", err.Error())
		return err
	}
	runInContainers(containers, func(c *container, _ chan *container) error {
		unit := c.asUnit(app)
		err := app.UnbindUnit(&unit)
		if err != nil {
			log.Errorf("Unable to unbind unit %q: %s", c.ID, err)
		}
		err = p.removeContainer(c, app)
		if err != nil {
			log.Errorf("Unable to destroy container %s: %s", c.ID, err.Error())
		}
		return nil
	}, nil, true)
	images, err := listAppImages(app.GetName())
	if err != nil {
		log.Errorf("Failed to get image ids for app %s: %s", app.GetName(), err.Error())
	}
	cluster := p.getCluster()
	for _, imageId := range images {
		err := cluster.RemoveImage(imageId)
		if err != nil {
			log.Errorf("Failed to remove image %s: %s", imageId, err.Error())
		}
		err = cluster.RemoveFromRegistry(imageId)
		if err != nil {
			log.Errorf("Failed to remove image %s from registry: %s", imageId, err.Error())
		}
	}
	err = deleteAllAppImageNames(app.GetName())
	if err != nil {
		log.Errorf("Failed to remove image names from storage for app %s: %s", app.GetName(), err.Error())
	}
	r, err := getRouterForApp(app)
	if err != nil {
		log.Errorf("Failed to get router: %s", err.Error())
		return err
	}
	err = r.RemoveBackend(app.GetName())
	if err != nil {
		log.Errorf("Failed to remove route backend: %s", err.Error())
		return err
	}
	return nil
}

func (*dockerProvisioner) Addr(app provision.App) (string, error) {
	r, err := getRouterForApp(app)
	if err != nil {
		log.Errorf("Failed to get router: %s", err)
		return "", err
	}
	addr, err := r.Addr(app.GetName())
	if err != nil {
		log.Errorf("Failed to obtain app %s address: %s", app.GetName(), err)
		return "", err
	}
	return addr, nil
}

func (p *dockerProvisioner) runRestartAfterHooks(cont *container, w io.Writer) error {
	yamlData, err := getImageTsuruYamlDataWithFallback(cont.Image, cont.AppName)
	if err != nil {
		return err
	}
	cmds := yamlData.Hooks.Restart.After
	for _, cmd := range cmds {
		err := cont.exec(p, w, w, cmd)
		if err != nil {
			return fmt.Errorf("couldn't execute restart:after hook %q(%s): %s", cmd, cont.shortID(), err.Error())
		}
	}
	return nil
}

func addContainersWithHost(args *changeUnitsPipelineArgs) ([]container, error) {
	a := args.app
	w := args.writer
	units := args.unitsToAdd
	imageId := args.imageId
	var destinationHost []string
	if args.toHost != "" {
		destinationHost = []string{args.toHost}
	}
	if w == nil {
		w = ioutil.Discard
	}
	wg := sync.WaitGroup{}
	createdContainers := make(chan *container, units)
	errors := make(chan error, units)
	var plural string
	if units > 1 {
		plural = "s"
	}
	fmt.Fprintf(w, "\n---- Starting %d new unit%s ----\n", units, plural)
	workers, _ := config.GetInt("docker:max-workers")
	if workers == 0 {
		workers = units
	}
	step := units/workers + 1
	for i := 0; i < units; i += step {
		wg.Add(1)
		last := i + step
		if last > units {
			last = units
		}
		go func(amount int) {
			defer wg.Done()
			for i := 0; i < amount; i++ {
				c, err := args.provisioner.start(a, imageId, w, destinationHost...)
				if err != nil {
					errors <- err
					return
				}
				createdContainers <- c
				fmt.Fprintf(w, " ---> Started unit %s...\n", c.shortID())
			}
		}(last - i)
	}
	wg.Wait()
	close(errors)
	close(createdContainers)
	if err := <-errors; err != nil {
		for c := range createdContainers {
			log.Errorf("Removing container %q due failed add units: %s", c.ID, err)
			errRem := args.provisioner.removeContainer(c, a)
			if errRem != nil {
				log.Errorf("Unable to destroy container %q: %s - %s", c.ID, err, errRem)
			}
		}
		return nil, err
	}
	result := make([]container, units)
	i := 0
	for c := range createdContainers {
		result[i] = *c
		i++
	}
	return result, nil
}

func (p *dockerProvisioner) AddUnits(a provision.App, units uint, w io.Writer) ([]provision.Unit, error) {
	if a.GetDeploys() == 0 {
		return nil, errors.New("New units can only be added after the first deployment")
	}
	if units == 0 {
		return nil, errors.New("Cannot add 0 units")
	}
	if w == nil {
		w = ioutil.Discard
	}
	writer := &app.LogWriter{App: a, Writer: w}
	imageId, err := appCurrentImageName(a.GetName())
	if err != nil {
		return nil, err
	}
	conts, err := p.runCreateUnitsPipeline(writer, a, int(units), imageId)
	if err != nil {
		return nil, err
	}
	result := make([]provision.Unit, len(conts))
	for i, c := range conts {
		result[i] = c.asUnit(a)
	}
	return result, nil
}

func (p *dockerProvisioner) RemoveUnits(a provision.App, units uint) error {
	if a == nil {
		return errors.New("remove units: app should not be nil")
	}
	if units < 1 {
		return errors.New("remove units: units must be at least 1")
	}
	countUnits, err := p.getContainerCountForAppName(a.GetName())
	if err != nil {
		return err
	}
	if int(units) > countUnits {
		return errors.New(fmt.Sprintf("remove units: cannot remove %d units. App %s has just %d units.", units, a.GetName(), countUnits))
	}
	for i := 0; i < int(units); i++ {
		containerID, err := p.scheduler.GetRemovableContainer(a.GetName(), p.cluster)
		if err != nil {
			return err
		}
		c, err := p.getContainer(containerID)
		if err != nil {
			return err
		}
		unit := c.asUnit(a)
		err = a.UnbindUnit(&unit)
		if err != nil {
			log.Errorf("Failed to unbind unit %q: %s", c.ID, err)
		}
		err = p.removeContainer(c, a)
		if err != nil {
			log.Errorf("Failed to remove container %q: %s", c.ID, err)
		}
	}
	return nil
}

func (p *dockerProvisioner) RemoveUnit(unit provision.Unit) error {
	container, err := p.getContainer(unit.Name)
	if err != nil {
		return err
	}
	a, err := container.getApp()
	if err != nil {
		return err
	}
	err = a.UnbindUnit(&unit)
	if err != nil {
		log.Errorf("Failed to unbind unit %q: %s", container.ID, err)
	}
	return p.removeContainer(container, a)
}

func (p *dockerProvisioner) removeContainer(c *container, a provision.App) error {
	if r, err := getRouterForApp(a); err == nil {
		if err := r.RemoveRoute(c.AppName, c.getAddress()); err != nil {
			log.Errorf("Failed to remove route: %s", err)
		}
	} else {
		log.Errorf("Failed to obtain router: %s", err)
	}
	err := c.remove(p)
	if err != nil {
		log.Errorf("error on remove container %s - %s", c.ID, err)
	}
	return err
}

func (p *dockerProvisioner) SetUnitStatus(unit provision.Unit, status provision.Status) error {
	container, err := p.getContainer(unit.Name)
	if err != nil {
		return err
	}
	if container.AppName != unit.AppName {
		return errors.New("wrong app name")
	}
	return container.setStatus(p, status.String())
}

func (p *dockerProvisioner) ExecuteCommandOnce(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	containers, err := p.listRunnableContainersByApp(app.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return provision.ErrEmptyApp
	}
	container := containers[0]
	return container.exec(p, stdout, stderr, cmd, args...)
}

func (p *dockerProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	containers, err := p.listRunnableContainersByApp(app.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return provision.ErrEmptyApp
	}
	for _, c := range containers {
		err = c.exec(p, stdout, stderr, cmd, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *dockerProvisioner) SetCName(app provision.App, cname string) error {
	r, err := getRouterForApp(app)
	if err != nil {
		return err
	}
	return r.SetCName(cname, app.GetName())
}

func (p *dockerProvisioner) UnsetCName(app provision.App, cname string) error {
	r, err := getRouterForApp(app)
	if err != nil {
		return err
	}
	return r.UnsetCName(cname, app.GetName())
}

func (p *dockerProvisioner) AdminCommands() []cmd.Command {
	return []cmd.Command{
		&moveContainerCmd{},
		&moveContainersCmd{},
		&rebalanceContainersCmd{},
		&addNodeToSchedulerCmd{},
		&removeNodeFromSchedulerCmd{},
		&listNodesInTheSchedulerCmd{},
		fixContainersCmd{},
		&listHealingHistoryCmd{},
		&listAutoScaleHistoryCmd{},
		&updateNodeToSchedulerCmd{},
		&listAutoScaleRunCmd{},
	}
}

func (p *dockerProvisioner) collection() *storage.Collection {
	conn, err := db.Conn()
	if err != nil {
		log.Errorf("Failed to connect to the database: %s", err)
	}
	return conn.Collection(p.collectionName)
}

// PlatformAdd build and push a new docker platform to register
func (p *dockerProvisioner) PlatformAdd(name string, args map[string]string, w io.Writer) error {
	if args["dockerfile"] == "" {
		return errors.New("Dockerfile is required.")
	}
	if _, err := url.ParseRequestURI(args["dockerfile"]); err != nil {
		return errors.New("dockerfile parameter should be an url.")
	}
	imageName := platformImageName(name)
	cluster := p.getCluster()
	buildOptions := docker.BuildImageOptions{
		Name:           imageName,
		NoCache:        true,
		RmTmpContainer: true,
		Remote:         args["dockerfile"],
		InputStream:    nil,
		OutputStream:   w,
	}
	err := cluster.BuildImage(buildOptions)
	if err != nil {
		return err
	}
	parts := strings.Split(imageName, ":")
	var tag string
	if len(parts) > 2 {
		imageName = strings.Join(parts[:len(parts)-1], ":")
		tag = parts[len(parts)-1]
	} else if len(parts) > 1 {
		imageName = parts[0]
		tag = parts[1]
	} else {
		imageName = parts[0]
		tag = "latest"
	}
	return p.pushImage(imageName, tag)
}

func (p *dockerProvisioner) PlatformUpdate(name string, args map[string]string, w io.Writer) error {
	return p.PlatformAdd(name, args, w)
}

func (p *dockerProvisioner) PlatformRemove(name string) error {
	err := p.getCluster().RemoveImage(platformImageName(name))
	if err != nil && err == docker.ErrNoSuchImage {
		log.Errorf("error on remove image %s from docker.", name)
		return nil
	}
	return err
}

func (p *dockerProvisioner) Units(app provision.App) []provision.Unit {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		return nil
	}
	units := []provision.Unit{}
	for _, container := range containers {
		unit := unitFromContainer(container)
		units = append(units, unit)
	}
	return units
}

func (p *dockerProvisioner) RegisterUnit(unit provision.Unit, customData map[string]interface{}) error {
	container, err := p.getContainer(unit.Name)
	if err != nil {
		return err
	}
	if container.Status == provision.StatusBuilding.String() {
		if container.BuildingImage != "" && customData != nil {
			return saveImageCustomData(container.BuildingImage, customData)
		}
		return nil
	}
	err = container.setStatus(p, provision.StatusStarted.String())
	if err != nil {
		return err
	}
	return p.checkContainer(container)
}

func (p *dockerProvisioner) Shell(opts provision.ShellOptions) error {
	var (
		c   *container
		err error
	)
	if opts.Unit != "" {
		c, err = p.getContainer(opts.Unit)
	} else {
		c, err = p.getOneContainerByAppName(opts.App.GetName())
	}
	if err != nil {
		return err
	}
	return c.shell(p, opts.Conn, opts.Conn, opts.Conn, pty{width: opts.Width, height: opts.Height, term: opts.Term})
}

func (p *dockerProvisioner) ValidAppImages(appName string) ([]string, error) {
	return listValidAppImages(appName)
}

func (p *dockerProvisioner) Nodes(app provision.App) ([]cluster.Node, error) {
	pool := app.GetPool()
	var (
		pools []provision.Pool
		err   error
	)
	if pool == "" {
		pools, err = provision.ListPools(bson.M{"$or": []bson.M{{"teams": app.GetTeamOwner()}, {"teams": bson.M{"$in": app.GetTeamsName()}}}})
	} else {
		pools, err = provision.ListPools(bson.M{"_id": pool})
	}
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		query := bson.M{"$or": []bson.M{{"teams": bson.M{"$exists": false}}, {"teams": bson.M{"$size": 0}}}}
		pools, err = provision.ListPools(query)
		if err != nil {
			return nil, err
		}
	}
	if len(pools) == 0 {
		return nil, errNoFallback
	}
	for _, pool := range pools {
		nodes, err := p.getCluster().NodesForMetadata(map[string]string{"pool": pool.Name})
		if err != nil {
			return nil, errNoFallback
		}
		if len(nodes) > 0 {
			return nodes, nil
		}
	}
	var nameList []string
	for _, pool := range pools {
		nameList = append(nameList, pool.Name)
	}
	poolsStr := strings.Join(nameList, ", pool=")
	return nil, fmt.Errorf("No nodes found with one of the following metadata: pool=%s", poolsStr)
}
