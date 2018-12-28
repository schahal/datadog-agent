// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build containerd

package containers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/cgroups"
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/typeurl"
	"github.com/gogo/protobuf/types"
	"gopkg.in/yaml.v2"

	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/autodiscovery/integration"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/DataDog/datadog-agent/pkg/collector/corechecks"
	core "github.com/DataDog/datadog-agent/pkg/collector/corechecks"
	"github.com/DataDog/datadog-agent/pkg/metrics"
	"github.com/DataDog/datadog-agent/pkg/tagger"
	"github.com/DataDog/datadog-agent/pkg/util"
	cutil "github.com/DataDog/datadog-agent/pkg/util/containerd"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

const (
	containerdCheckName = "containerd"
)

// ContainerCheck grabs containerd metrics and events
type ContainerdCheck struct {
	core.CheckBase
	hostname string
	instance *ContainerdConfig
	sub      *Subscriber
}

// ContainerdConfig contains the custom options and configurations set by the user.
type ContainerdConfig struct {
	Tags    []string `yaml:"tags"`
	Filters []string `yaml:"filters"`
}

func init() {
	corechecks.RegisterCheck(containerdCheckName, ContainerdFactory)
}

// ContainerdFactory is used to create register the check and initialize it.
func ContainerdFactory() check.Check {
	return &ContainerdCheck{
		CheckBase: corechecks.NewCheckBase(containerdCheckName),
		instance:  &ContainerdConfig{},
		sub:       &Subscriber{},
	}
}

// Parse is used to get the configuration set by the user
func (co *ContainerdConfig) Parse(data []byte) error {
	if err := yaml.Unmarshal(data, co); err != nil {
		return err
	}
	return nil
}

// Configure parses the check configuration and init the check
func (c *ContainerdCheck) Configure(config, initConfig integration.Data) error {
	var err error
	if err = c.CommonConfigure(config); err != nil {
		return err
	}

	c.hostname, err = util.GetHostname()
	if err != nil {
		return err
	}

	if err = c.instance.Parse(config); err != nil {
		return err
	}
	c.sub.Filters = c.instance.Filters
	return nil
}

// Run executes the check
func (c *ContainerdCheck) Run() error {
	sender, err := aggregator.GetSender(c.ID())
	defer sender.Commit()
	if err != nil {
		return err
	}
	// As we do not rely on a singleton, we ensure connectivity every check run.
	cu, errHealth := cutil.GetContainerdUtil()
	if errHealth != nil {
		log.Infof("Error ensuring connectivity with Containerd daemon %v", errHealth)
		return errHealth
	}
	ns := cu.Namespace()

	if c.sub == nil {
		c.sub = CreateEventSubscriber("ContainerdCheck", ns, c.instance.Filters)
	}

	if !c.sub.IsRunning() {
		// Keep track of the health of the Containerd socket
		c.sub.CheckEvents(cu)
	}
	events := c.sub.Flush(time.Now().Unix())
	// Process events
	computeEvents(c.hostname, events, sender, c.instance.Tags)

	nk := namespaces.WithNamespace(context.Background(), ns)
	computeMetrics(sender, nk, cu, c.instance.Tags)

	return nil
}

// compute events converts Containerd events into Datadog events
func computeEvents(hostname string, events []ContainerdEvent, sender aggregator.Sender, userTags []string) {
	for _, e := range events {
		split := strings.Split(e.Topic, "/")
		if len(split) != 3 {
			// sanity checking the event, to avoid submitting
			log.Tracef("Event topic %s does not have the expected format", e.Topic)
			continue
		}

		output := metrics.Event{
			Priority:       metrics.EventPriorityNormal,
			Host:           hostname,
			SourceTypeName: containerdCheckName,
			EventType:      containerdCheckName,
			AggregationKey: fmt.Sprintf("containerd:%s", e.Topic),
		}
		output.Text = e.Message
		if len(e.Extra) > 0 {
			for k, v := range e.Extra {
				output.Tags = append(output.Tags, fmt.Sprintf("%s:%s", k, v))
			}
		}
		output.Tags = append(output.Tags, userTags...)
		output.Ts = e.Timestamp.Unix()
		output.Title = fmt.Sprintf("Event on %s from Containerd", split[1])
		if split[1] == "containers" || split[1] == "tasks" {
			// For task events, we use the container ID in order to query the Tagger's API
			tags, err := tagger.Tag(e.ID, true)
			if err != nil {
				// If there is an error retrieving tags from the Tagger, we can still submit the event as is.
				log.Errorf("Could not retrieve tags for the container %s: %v", e.ID, err)
			}
			output.Tags = append(output.Tags, tags...)
		}
		sender.Event(output)
	}
}

func computeMetrics(sender aggregator.Sender, nk context.Context, cu cutil.ContainerdItf, userTags []string) {
	containers, err := cu.Containers()
	if err != nil {
		log.Errorf(err.Error())
		return
	}

	for _, ctn := range containers {
		t, errTask := ctn.Task(nk, nil)
		if errTask != nil {
			log.Tracef("Could not retrieve metrics from task %s: %s", ctn.ID()[:12], errTask.Error())
			continue
		}

		tags, err := collectTags(ctn, nk)
		if err != nil {
			log.Errorf("Could not collect tags for container %s: %s", ctn.ID()[:12], err)
		}
		tags = append(tags, userTags...)
		// Tagger tags
		taggerTags, err := tagger.Tag(ctn.ID(), false)
		if err != nil {
			log.Errorf(err.Error())
			continue
		}
		tags = append(tags, taggerTags...)

		metrics, err := convertTasktoMetrics(t, nk)
		if err != nil {
			log.Errorf("Could not process the metrics from %s: %v", ctn.ID(), err.Error())
			continue
		}

		err = computeExtra(sender, ctn, nk, tags)
		if err != nil {
			log.Errorf("Could not process metadata related metrics for %s: %v", ctn.ID()[:12], err.Error())
		}

		if metrics.Memory.Size() > 0 {
			computeMem(sender, metrics.Memory, tags)
		}

		if metrics.CPU.Throttling != nil && metrics.CPU.Usage != nil {
			computeCPU(sender, metrics.CPU, tags)
		}

		if metrics.Blkio.Size() > 0 {
			computeBlkio(sender, metrics.Blkio, tags)
		}

		if len(metrics.Hugetlb) > 0 {
			computeHugetlb(sender, metrics.Hugetlb, tags)
		}
	}
}

func convertTasktoMetrics(task containerd.Task, nsCtx context.Context) (*cgroups.Metrics, error) {
	metricTask, err := task.Metrics(nsCtx)
	if err != nil {
		return nil, err
	}

	metricAny, err := typeurl.UnmarshalAny(&types.Any{
		TypeUrl: metricTask.Data.TypeUrl,
		Value:   metricTask.Data.Value,
	})
	if err != nil {
		log.Errorf(err.Error())
		return nil, err
	}
	return metricAny.(*cgroups.Metrics), nil
}

func computeExtra(sender aggregator.Sender, ctn containerd.Container, nsCtx context.Context, tags []string) error {
	img, err := ctn.Image(nsCtx)
	if err != nil {
		return err
	}
	size, err := img.Size(nsCtx)
	if err != nil {
		return err
	}
	sender.Gauge("containerd.image.size", float64(size), "", tags)
	return nil
}

func collectTags(ctn containerd.Container, nsCtx context.Context) ([]string, error) {
	tags := []string{}

	// Container image
	im, err := ctn.Image(nsCtx)
	if err != nil {
		return tags, err
	}
	imageName := fmt.Sprintf("image:%s", im.Name())
	tags = append(tags, imageName)

	// Container labels
	labels, err := ctn.Labels(nsCtx)
	if err != nil {
		return tags, err
	}
	for k, v := range labels {
		tag := fmt.Sprintf("%s:%s", k, v)
		tags = append(tags, tag)
	}

	// Container meta
	i, err := ctn.Info(nsCtx)
	if err != nil {
		return tags, err
	}

	runt := fmt.Sprintf("runtime:%s", i.Runtime.Name)
	tags = append(tags, runt)

	return tags, nil
}

func computeHugetlb(sender aggregator.Sender, huge []*cgroups.HugetlbStat, tags []string) {
	for _, h := range huge {
		sender.Gauge("containerd.hugetlb.max", float64(h.Max), "", tags)
		sender.Gauge("containerd.hugetlb.failcount", float64(h.Failcnt), "", tags)
		sender.Gauge("containerd.hugetlb.usage", float64(h.Usage), "", tags)
	}
}

func computeMem(sender aggregator.Sender, mem *cgroups.MemoryStat, tags []string) {
	memList := map[string]*cgroups.MemoryEntry{
		"containerd.mem.current":    mem.Usage,
		"containerd.mem.kernel_tcp": mem.KernelTCP,
		"containerd.mem.kernel":     mem.Kernel,
		"containerd.mem.swap":       mem.Swap,
	}
	for metricName, memStat := range memList {
		parseAndSubmitMem(metricName, sender, memStat, tags)
	}
	sender.Gauge("containerd.mem.cache", float64(mem.Cache), "", tags)
	sender.Gauge("containerd.mem.rss", float64(mem.RSS), "", tags)
	sender.Gauge("containerd.mem.rsshuge", float64(mem.RSSHuge), "", tags)
	sender.Gauge("containerd.mem.usage", float64(mem.Usage.Usage), "", tags)
	sender.Gauge("containerd.mem.kernel.usage", float64(mem.Kernel.Usage), "", tags)
	sender.Gauge("containerd.mem.dirty", float64(mem.Dirty), "", tags)
}

func parseAndSubmitMem(metricName string, sender aggregator.Sender, stat *cgroups.MemoryEntry, tags []string) {
	if stat.Size() == 0 {
		return
	}
	sender.Gauge(fmt.Sprintf("%s.usage", metricName), float64(stat.Usage), "", tags)
	sender.Gauge(fmt.Sprintf("%s.failcnt", metricName), float64(stat.Failcnt), "", tags)
	sender.Gauge(fmt.Sprintf("%s.limit", metricName), float64(stat.Limit), "", tags)
	sender.Gauge(fmt.Sprintf("%s.max", metricName), float64(stat.Max), "", tags)

}

func computeCPU(sender aggregator.Sender, cpu *cgroups.CPUStat, tags []string) {
	sender.Gauge("containerd.cpu.system", float64(cpu.Usage.Kernel), "", tags)
	sender.Gauge("containerd.cpu.total", float64(cpu.Usage.Total), "", tags)
	sender.Gauge("containerd.cpu.user", float64(cpu.Usage.User), "", tags)
	sender.Gauge("containerd.cpu.throttle.periods", float64(cpu.Throttling.Periods), "", tags)
}

func computeBlkio(sender aggregator.Sender, blkio *cgroups.BlkIOStat, tags []string) {
	blkioList := map[string][]*cgroups.BlkIOEntry{
		"containerd.blkio.merged_recursive":        blkio.IoMergedRecursive,
		"containerd.blkio.queued_recursive":        blkio.IoQueuedRecursive,
		"containerd.blkio.sectors_recursive":       blkio.SectorsRecursive,
		"containerd.blkio.service_recursive_bytes": blkio.IoServiceBytesRecursive,
		"containerd.blkio.time_recursive":          blkio.IoTimeRecursive,
		"containerd.blkio.serviced_recursive":      blkio.IoServicedRecursive,
		"containerd.blkio.wait_time_recursive":     blkio.IoWaitTimeRecursive,
		"containerd.blkio.service_time_recursive":  blkio.IoServiceTimeRecursive,
	}
	for metricName, ioStats := range blkioList {
		parseAndSubmitBlkio(metricName, sender, ioStats, tags)
	}
}

func parseAndSubmitBlkio(metricName string, sender aggregator.Sender, list []*cgroups.BlkIOEntry, tags []string) {
	for _, m := range list {
		if m.Size() == 0 {
			continue
		}
		blkiotags := []string{
			fmt.Sprintf("dev:%s", m.Device),
		}
		if m.Op != "" {
			blkiotags = append(blkiotags, fmt.Sprintf("operation:%s", m.Op))
		}

		tags = append(tags, blkiotags...)
		sender.Gauge(metricName, float64(m.Value), "", tags)
	}
}
