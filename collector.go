package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

var labelCname = []string{"container_name"}

type DockerCollector struct {
	cli         *client.Client
	containerRe *regexp.Regexp
}

func newDockerCollector() *DockerCollector {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("can't create docker client: %v", err)
	}
	container_name_regex, found := os.LookupEnv("DEX_FILTER_CONTAINER")
	if !found {
		container_name_regex = ".*"
	}

	re, err := regexp.Compile(container_name_regex)
	if err != nil {
		log.Fatalf("invalid container filter regexp '%s': %v", container_name_regex, err)
	}

	return &DockerCollector{
		cli:         cli,
		containerRe: re,
	}
}

func (c *DockerCollector) Describe(_ chan<- *prometheus.Desc) {

}

func (c *DockerCollector) Collect(ch chan<- prometheus.Metric) {
	containers, err := c.cli.ContainerList(context.Background(), container.ListOptions{
		All: true,
	})
	if err != nil {
		log.Error("can't list containers: ", err)
		return
	}

	var wg sync.WaitGroup

	for _, container := range containers {
		wg.Add(1)

		go c.processContainer(container, ch, &wg)
	}
	wg.Wait()
}

func (c *DockerCollector) processContainer(cont container.Summary, ch chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()

	cName := strings.TrimPrefix(strings.Join(cont.Names, ";"), "/")
	submatches := c.containerRe.FindStringSubmatch(cName)
	if len(submatches) == 0 {
		return
	}
	cName = submatches[len(submatches)-1]

	var isRunning, isRestarting, isExited float64

	if cont.State == "running" {
		isRunning = 1
	}

	if cont.State == "restarting" {
		isRestarting = 1
	}

	if cont.State == "exited" {
		isExited = 1
	}

	// container state metric for all containers
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_container_running",
		"1 if docker container is running, 0 otherwise",
		labelCname,
		nil,
	), prometheus.GaugeValue, isRunning, cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_container_restarting",
		"1 if docker container is restarting, 0 otherwise",
		labelCname,
		nil,
	), prometheus.GaugeValue, isRestarting, cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_container_exited",
		"1 if docker container exited, 0 otherwise",
		labelCname,
		nil,
	), prometheus.GaugeValue, isExited, cName)

	if inspect, err := c.cli.ContainerInspect(context.Background(), cont.ID); err != nil {
		log.Fatal(err)
	} else {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
			"dex_container_restarts_total",
			"Number of times the container has restarted",
			labelCname,
			nil,
		), prometheus.CounterValue, float64(inspect.RestartCount), cName)
	}

	// stats metrics only for running containers
	if isRunning == 1 {

		if stats, err := c.cli.ContainerStats(context.Background(), cont.ID, false); err != nil {
			log.Fatal(err)
		} else {
			var containerStats container.StatsResponse
			err := json.NewDecoder(stats.Body).Decode(&containerStats)
			if err != nil {
				log.Error("can't read api stats: ", err)
			}
			if err := stats.Body.Close(); err != nil {
				log.Error("can't close body: ", err)
			}

			c.blockIoMetrics(ch, &containerStats, cName)

			c.memoryMetrics(ch, &containerStats, cName)

			c.networkMetrics(ch, &containerStats, cName)

			c.CPUMetrics(ch, &containerStats, cName)

			c.pidsMetrics(ch, &containerStats, cName)
		}
	}
}

func (c *DockerCollector) CPUMetrics(ch chan<- prometheus.Metric, containerStats *container.StatsResponse, cName string) {
	totalUsage := containerStats.CPUStats.CPUUsage.TotalUsage
	cpuDelta := totalUsage - containerStats.PreCPUStats.CPUUsage.TotalUsage
	sysemDelta := containerStats.CPUStats.SystemUsage - containerStats.PreCPUStats.SystemUsage

	cpuUtilization := float64(cpuDelta) / float64(sysemDelta) * 100.0

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_cpu_utilization_percent",
		"CPU utilization in percent",
		labelCname,
		nil,
	), prometheus.GaugeValue, cpuUtilization, cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_cpu_utilization_seconds_total",
		"Cumulative CPU utilization in seconds",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(totalUsage)/1e9, cName)
}

func (c *DockerCollector) networkMetrics(ch chan<- prometheus.Metric, containerStats *container.StatsResponse, cName string) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_network_rx_bytes_total",
		"Network received bytes total",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.Networks["eth0"].RxBytes), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_network_tx_bytes_total",
		"Network sent bytes total",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.Networks["eth0"].TxBytes), cName)
}

func (c *DockerCollector) memoryMetrics(ch chan<- prometheus.Metric, containerStats *container.StatsResponse, cName string) {
	// From official documentation
	//Note: On Linux, the Docker CLI reports memory usage by subtracting page cache usage from the total memory usage.
	//The API does not perform such a calculation but rather provides the total memory usage and the amount from the page cache so that clients can use the data as needed.
	memoryUsage := containerStats.MemoryStats.Usage - containerStats.MemoryStats.Stats["cache"]
	memoryTotal := containerStats.MemoryStats.Limit

	memoryUtilization := float64(memoryUsage) / float64(memoryTotal) * 100.0
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_usage_bytes",
		"Total memory usage bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(memoryUsage), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_total_bytes",
		"Total memory bytes",
		labelCname,
		nil,
	), prometheus.GaugeValue, float64(memoryTotal), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_utilization_percent",
		"Memory utilization percent",
		labelCname,
		nil,
	), prometheus.GaugeValue, memoryUtilization, cName)
}

func (c *DockerCollector) blockIoMetrics(ch chan<- prometheus.Metric, containerStats *container.StatsResponse, cName string) {
	var readTotal, writeTotal uint64
	for _, b := range containerStats.BlkioStats.IoServiceBytesRecursive {
		if strings.EqualFold(b.Op, "read") {
			readTotal += b.Value
		}
		if strings.EqualFold(b.Op, "write") {
			writeTotal += b.Value
		}
	}

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_block_io_read_bytes_total",
		"Block I/O read bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(readTotal), cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_block_io_write_bytes_total",
		"Block I/O write bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(writeTotal), cName)
}

func (c *DockerCollector) pidsMetrics(ch chan<- prometheus.Metric, containerStats *container.StatsResponse, cName string) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_pids_current",
		"Current number of pids in the cgroup",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.PidsStats.Current), cName)
}
