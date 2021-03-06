// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
)

const (
	defaultHelp = "Metric autogenerated by statsd_exporter."
	regErrF     = "A change of configuration created inconsistent metrics for " +
		"%q. You have to restart the statsd_exporter, and you should " +
		"consider the effects on your monitoring setup. Error: %s"
)

var (
	illegalCharsRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

	hash   = fnv.New64a()
	strBuf bytes.Buffer // Used for hashing.
	intBuf = make([]byte, 8)
)

// hashNameAndLabels returns a hash value of the provided name string and all
// the label names and values in the provided labels map.
//
// Not safe for concurrent use! (Uses a shared buffer and hasher to save on
// allocations.)
func hashNameAndLabels(name string, labels prometheus.Labels) uint64 {
	hash.Reset()
	strBuf.Reset()
	strBuf.WriteString(name)
	hash.Write(strBuf.Bytes())
	binary.BigEndian.PutUint64(intBuf, model.LabelsToSignature(labels))
	hash.Write(intBuf)
	return hash.Sum64()
}

type CounterContainer struct {
	Elements map[uint64]prometheus.Counter
}

func NewCounterContainer() *CounterContainer {
	return &CounterContainer{
		Elements: make(map[uint64]prometheus.Counter),
	}
}

func (c *CounterContainer) Get(metricName string, labels prometheus.Labels, help string) (prometheus.Counter, error) {
	hash := hashNameAndLabels(metricName, labels)
	counter, ok := c.Elements[hash]
	if !ok {
		counter = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        metricName,
			Help:        help,
			ConstLabels: labels,
		})
		if err := prometheus.Register(counter); err != nil {
			return nil, err
		}
		c.Elements[hash] = counter
	}
	return counter, nil
}

type GaugeContainer struct {
	Elements map[uint64]prometheus.Gauge
}

func NewGaugeContainer() *GaugeContainer {
	return &GaugeContainer{
		Elements: make(map[uint64]prometheus.Gauge),
	}
}

func (c *GaugeContainer) Get(metricName string, labels prometheus.Labels, help string) (prometheus.Gauge, error) {
	hash := hashNameAndLabels(metricName, labels)
	gauge, ok := c.Elements[hash]
	if !ok {
		gauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricName,
			Help:        help,
			ConstLabels: labels,
		})
		if err := prometheus.Register(gauge); err != nil {
			return nil, err
		}
		c.Elements[hash] = gauge
	}
	return gauge, nil
}

type SummaryContainer struct {
	Elements map[uint64]prometheus.Summary
	mapper   *metricMapper
}

func NewSummaryContainer(mapper *metricMapper) *SummaryContainer {
	return &SummaryContainer{
		Elements: make(map[uint64]prometheus.Summary),
		mapper:   mapper,
	}
}

func (c *SummaryContainer) Get(metricName string, labels prometheus.Labels, help string, mapping *metricMapping) (prometheus.Summary, error) {
	hash := hashNameAndLabels(metricName, labels)
	summary, ok := c.Elements[hash]
	if !ok {
		quantiles := c.mapper.Defaults.Quantiles
		if mapping != nil && mapping.Quantiles != nil && len(mapping.Quantiles) > 0 {
			quantiles = mapping.Quantiles
		}
		objectives := make(map[float64]float64)
		for _, q := range quantiles {
			objectives[q.Quantile] = q.Error
		}
		summary = prometheus.NewSummary(
			prometheus.SummaryOpts{
				Name:        metricName,
				Help:        help,
				ConstLabels: labels,
				Objectives:  objectives,
			})
		if err := prometheus.Register(summary); err != nil {
			return nil, err
		}
		c.Elements[hash] = summary
	}
	return summary, nil
}

type HistogramContainer struct {
	Elements map[uint64]prometheus.Histogram
	mapper   *metricMapper
}

func NewHistogramContainer(mapper *metricMapper) *HistogramContainer {
	return &HistogramContainer{
		Elements: make(map[uint64]prometheus.Histogram),
		mapper:   mapper,
	}
}

func (c *HistogramContainer) Get(metricName string, labels prometheus.Labels, help string, mapping *metricMapping) (prometheus.Histogram, error) {
	hash := hashNameAndLabels(metricName, labels)
	histogram, ok := c.Elements[hash]
	if !ok {
		buckets := c.mapper.Defaults.Buckets
		if mapping != nil && mapping.Buckets != nil && len(mapping.Buckets) > 0 {
			buckets = mapping.Buckets
		}
		histogram = prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:        metricName,
				Help:        help,
				ConstLabels: labels,
				Buckets:     buckets,
			})
		c.Elements[hash] = histogram
		if err := prometheus.Register(histogram); err != nil {
			return nil, err
		}
	}
	return histogram, nil
}

type Event interface {
	MetricName() string
	Value() float64
	Labels() map[string]string
	MetricType() metricType
}

type CounterEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (c *CounterEvent) MetricName() string        { return c.metricName }
func (c *CounterEvent) Value() float64            { return c.value }
func (c *CounterEvent) Labels() map[string]string { return c.labels }
func (c *CounterEvent) MetricType() metricType    { return metricTypeCounter }

type GaugeEvent struct {
	metricName string
	value      float64
	relative   bool
	labels     map[string]string
}

func (g *GaugeEvent) MetricName() string        { return g.metricName }
func (g *GaugeEvent) Value() float64            { return g.value }
func (c *GaugeEvent) Labels() map[string]string { return c.labels }
func (c *GaugeEvent) MetricType() metricType    { return metricTypeGauge }

type TimerEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (t *TimerEvent) MetricName() string        { return t.metricName }
func (t *TimerEvent) Value() float64            { return t.value }
func (c *TimerEvent) Labels() map[string]string { return c.labels }
func (c *TimerEvent) MetricType() metricType    { return metricTypeTimer }

type Events []Event

type Exporter struct {
	Counters   *CounterContainer
	Gauges     *GaugeContainer
	Summaries  *SummaryContainer
	Histograms *HistogramContainer
	mapper     *metricMapper
}

func escapeMetricName(metricName string) string {
	// If a metric starts with a digit, prepend an underscore.
	if metricName[0] >= '0' && metricName[0] <= '9' {
		metricName = "_" + metricName
	}

	// Replace all illegal metric chars with underscores.
	metricName = illegalCharsRE.ReplaceAllString(metricName, "_")
	return metricName
}

func (b *Exporter) Listen(e <-chan Events) {
	for {
		events, ok := <-e
		if !ok {
			log.Debug("Channel is closed. Break out of Exporter.Listener.")
			return
		}
		for _, event := range events {
			var help string
			metricName := ""
			prometheusLabels := event.Labels()

			mapping, labels, present := b.mapper.getMapping(event.MetricName(), event.MetricType())
			if mapping == nil {
				mapping = &metricMapping{}
			}

			if mapping.Action == actionTypeDrop {
				continue
			}

			if mapping.HelpText == "" {
				help = defaultHelp
			} else {
				help = mapping.HelpText
			}
			if present {
				metricName = escapeMetricName(mapping.Name)
				for label, value := range labels {
					prometheusLabels[label] = value
				}
			} else {
				eventsUnmapped.Inc()
				metricName = escapeMetricName(event.MetricName())
			}

			switch ev := event.(type) {
			case *CounterEvent:
				// We don't accept negative values for counters. Incrementing the counter with a negative number
				// will cause the exporter to panic. Instead we will warn and continue to the next event.
				if event.Value() < 0.0 {
					log.Debugf("Counter %q is: '%f' (counter must be non-negative value)", metricName, event.Value())
					eventStats.WithLabelValues("illegal_negative_counter").Inc()
					continue
				}

				counter, err := b.Counters.Get(
					metricName,
					prometheusLabels,
					help,
				)
				if err == nil {
					counter.Add(event.Value())

					eventStats.WithLabelValues("counter").Inc()
				} else {
					log.Debugf(regErrF, metricName, err)
					conflictingEventStats.WithLabelValues("counter").Inc()
				}

			case *GaugeEvent:
				gauge, err := b.Gauges.Get(
					metricName,
					prometheusLabels,
					help,
				)

				if err == nil {
					if ev.relative {
						gauge.Add(event.Value())
					} else {
						gauge.Set(event.Value())
					}

					eventStats.WithLabelValues("gauge").Inc()
				} else {
					log.Debugf(regErrF, metricName, err)
					conflictingEventStats.WithLabelValues("gauge").Inc()
				}

			case *TimerEvent:
				t := timerTypeDefault
				if mapping != nil {
					t = mapping.TimerType
				}
				if t == timerTypeDefault {
					t = b.mapper.Defaults.TimerType
				}

				switch t {
				case timerTypeHistogram:
					histogram, err := b.Histograms.Get(
						metricName,
						prometheusLabels,
						help,
						mapping,
					)
					if err == nil {
						histogram.Observe(event.Value() / 1000) // prometheus presumes seconds, statsd millisecond
						eventStats.WithLabelValues("timer").Inc()
					} else {
						log.Debugf(regErrF, metricName, err)
						conflictingEventStats.WithLabelValues("timer").Inc()
					}

				case timerTypeDefault, timerTypeSummary:
					summary, err := b.Summaries.Get(
						metricName,
						prometheusLabels,
						help,
						mapping,
					)
					if err == nil {
						summary.Observe(event.Value())
						eventStats.WithLabelValues("timer").Inc()
					} else {
						log.Debugf(regErrF, metricName, err)
						conflictingEventStats.WithLabelValues("timer").Inc()
					}

				default:
					panic(fmt.Sprintf("unknown timer type '%s'", t))
				}

			default:
				log.Debugln("Unsupported event type")
				eventStats.WithLabelValues("illegal").Inc()
			}
		}
	}
}

func NewExporter(mapper *metricMapper) *Exporter {
	return &Exporter{
		Counters:   NewCounterContainer(),
		Gauges:     NewGaugeContainer(),
		Summaries:  NewSummaryContainer(mapper),
		Histograms: NewHistogramContainer(mapper),
		mapper:     mapper,
	}
}

func buildEvent(statType, metric string, value float64, relative bool, labels map[string]string) (Event, error) {
	switch statType {
	case "c":
		return &CounterEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "g":
		return &GaugeEvent{
			metricName: metric,
			value:      float64(value),
			relative:   relative,
			labels:     labels,
		}, nil
	case "ms", "h":
		return &TimerEvent{
			metricName: metric,
			value:      float64(value),
			labels:     labels,
		}, nil
	case "s":
		return nil, fmt.Errorf("No support for StatsD sets")
	default:
		return nil, fmt.Errorf("Bad stat type %s", statType)
	}
}

func parseDogStatsDTagsToLabels(component string) map[string]string {
	labels := map[string]string{}
	tagsReceived.Inc()
	tags := strings.Split(component, ",")
	for _, t := range tags {
		t = strings.TrimPrefix(t, "#")
		kv := strings.SplitN(t, ":", 2)

		if len(kv) < 2 || len(kv[1]) == 0 {
			tagErrors.Inc()
			log.Debugf("Malformed or empty DogStatsD tag %s in component %s", t, component)
			continue
		}

		labels[escapeMetricName(kv[0])] = kv[1]
	}
	return labels
}

func lineToEvents(line string) Events {
	events := Events{}
	if line == "" {
		return events
	}

	elements := strings.SplitN(line, ":", 2)
	if len(elements) < 2 || len(elements[0]) == 0 || !utf8.ValidString(line) {
		sampleErrors.WithLabelValues("malformed_line").Inc()
		log.Debugln("Bad line from StatsD:", line)
		return events
	}
	metric := elements[0]
	var samples []string
	if strings.Contains(elements[1], "|#") {
		// using datadog extensions, disable multi-metrics
		samples = elements[1:]
	} else {
		samples = strings.Split(elements[1], ":")
	}
samples:
	for _, sample := range samples {
		samplesReceived.Inc()
		components := strings.Split(sample, "|")
		samplingFactor := 1.0
		if len(components) < 2 || len(components) > 4 {
			sampleErrors.WithLabelValues("malformed_component").Inc()
			log.Debugln("Bad component on line:", line)
			continue
		}
		valueStr, statType := components[0], components[1]

		var relative = false
		if strings.Index(valueStr, "+") == 0 || strings.Index(valueStr, "-") == 0 {
			relative = true
		}

		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Debugf("Bad value %s on line: %s", valueStr, line)
			sampleErrors.WithLabelValues("malformed_value").Inc()
			continue
		}

		multiplyEvents := 1
		labels := map[string]string{}
		if len(components) >= 3 {
			for _, component := range components[2:] {
				if len(component) == 0 {
					log.Debugln("Empty component on line: ", line)
					sampleErrors.WithLabelValues("malformed_component").Inc()
					continue samples
				}
			}

			for _, component := range components[2:] {
				switch component[0] {
				case '@':
					if statType != "c" && statType != "ms" {
						log.Debugln("Illegal sampling factor for non-counter metric on line", line)
						sampleErrors.WithLabelValues("illegal_sample_factor").Inc()
						continue
					}
					samplingFactor, err = strconv.ParseFloat(component[1:], 64)
					if err != nil {
						log.Debugf("Invalid sampling factor %s on line %s", component[1:], line)
						sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					}
					if samplingFactor == 0 {
						samplingFactor = 1
					}

					if statType == "c" {
						value /= samplingFactor
					} else if statType == "ms" {
						multiplyEvents = int(1 / samplingFactor)
					}
				case '#':
					labels = parseDogStatsDTagsToLabels(component)
				default:
					log.Debugf("Invalid sampling factor or tag section %s on line %s", components[2], line)
					sampleErrors.WithLabelValues("invalid_sample_factor").Inc()
					continue
				}
			}
		}

		for i := 0; i < multiplyEvents; i++ {
			event, err := buildEvent(statType, metric, value, relative, labels)
			if err != nil {
				log.Debugf("Error building event on line %s: %s", line, err)
				sampleErrors.WithLabelValues("illegal_event").Inc()
				continue
			}
			events = append(events, event)
		}
	}
	return events
}

type StatsDUDPListener struct {
	conn *net.UDPConn
}

func (l *StatsDUDPListener) Listen(e chan<- Events) {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			log.Fatal(err)
		}
		l.handlePacket(buf[0:n], e)
	}
}

func (l *StatsDUDPListener) handlePacket(packet []byte, e chan<- Events) {
	udpPackets.Inc()
	lines := strings.Split(string(packet), "\n")
	events := Events{}
	for _, line := range lines {
		linesReceived.Inc()
		events = append(events, lineToEvents(line)...)
	}
	e <- events
}

type StatsDTCPListener struct {
	conn *net.TCPListener
}

func (l *StatsDTCPListener) Listen(e chan<- Events) {
	for {
		c, err := l.conn.AcceptTCP()
		if err != nil {
			log.Fatalf("AcceptTCP failed: %v", err)
		}
		go l.handleConn(c, e)
	}
}

func (l *StatsDTCPListener) handleConn(c *net.TCPConn, e chan<- Events) {
	defer c.Close()

	tcpConnections.Inc()

	r := bufio.NewReader(c)
	for {
		line, isPrefix, err := r.ReadLine()
		if err != nil {
			if err != io.EOF {
				tcpErrors.Inc()
				log.Debugf("Read %s failed: %v", c.RemoteAddr(), err)
			}
			break
		}
		if isPrefix {
			tcpLineTooLong.Inc()
			log.Debugf("Read %s failed: line too long", c.RemoteAddr())
			break
		}
		linesReceived.Inc()
		e <- lineToEvents(string(line))
	}
}
