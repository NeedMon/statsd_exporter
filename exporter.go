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
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
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

func (c *CounterContainer) Get(metricName string, labels prometheus.Labels) prometheus.Counter {
	hash := hashNameAndLabels(metricName, labels)
	counter, ok := c.Elements[hash]
	if !ok {
		counter = prometheus.NewCounter(prometheus.CounterOpts{
			Name:        metricName,
			Help:        defaultHelp,
			ConstLabels: labels,
		})
		c.Elements[hash] = counter
		if err := prometheus.Register(counter); err != nil {
			log.Fatalf(regErrF, metricName, err)
		}
	}
	return counter
}

type GaugeContainer struct {
	Elements map[uint64]prometheus.Gauge
}

func NewGaugeContainer() *GaugeContainer {
	return &GaugeContainer{
		Elements: make(map[uint64]prometheus.Gauge),
	}
}

func (c *GaugeContainer) Get(metricName string, labels prometheus.Labels) prometheus.Gauge {
	hash := hashNameAndLabels(metricName, labels)
	gauge, ok := c.Elements[hash]
	if !ok {
		gauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        metricName,
			Help:        defaultHelp,
			ConstLabels: labels,
		})
		c.Elements[hash] = gauge
		if err := prometheus.Register(gauge); err != nil {
			log.Fatalf(regErrF, metricName, err)
		}
	}
	return gauge
}

type SummaryContainer struct {
	Elements map[uint64]prometheus.Summary
}

func NewSummaryContainer() *SummaryContainer {
	return &SummaryContainer{
		Elements: make(map[uint64]prometheus.Summary),
	}
}

func (c *SummaryContainer) Get(metricName string, labels prometheus.Labels) prometheus.Summary {
	hash := hashNameAndLabels(metricName, labels)
	summary, ok := c.Elements[hash]
	if !ok {
		summary = prometheus.NewSummary(
			prometheus.SummaryOpts{
				Name:        metricName,
				Help:        defaultHelp,
				ConstLabels: labels,
			})
		c.Elements[hash] = summary
		if err := prometheus.Register(summary); err != nil {
			log.Fatalf(regErrF, metricName, err)
		}
	}
	return summary
}

type Event interface {
	MetricName() string
	Value() float64
	Labels() map[string]string
}

type CounterEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (c *CounterEvent) MetricName() string        { return c.metricName }
func (c *CounterEvent) Value() float64            { return c.value }
func (c *CounterEvent) Labels() map[string]string { return c.labels }

type GaugeEvent struct {
	metricName string
	value      float64
	relative   bool
	labels     map[string]string
}

func (g *GaugeEvent) MetricName() string        { return g.metricName }
func (g *GaugeEvent) Value() float64            { return g.value }
func (c *GaugeEvent) Labels() map[string]string { return c.labels }

type TimerEvent struct {
	metricName string
	value      float64
	labels     map[string]string
}

func (t *TimerEvent) MetricName() string        { return t.metricName }
func (t *TimerEvent) Value() float64            { return t.value }
func (c *TimerEvent) Labels() map[string]string { return c.labels }

type Events []Event

type Exporter struct {
	Counters  *CounterContainer
	Gauges    *GaugeContainer
	Summaries *SummaryContainer
	mapper    *metricMapper
	addSuffix bool
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

func (b *Exporter) suffix(metricName, suffix string) string {
	str := metricName
	if b.addSuffix {
		str += "_" + suffix
	}
	return str
}

func (b *Exporter) Listen(e <-chan Events) {
	for {
		events, ok := <-e
		if !ok {
			log.Debug("Channel is closed. Break out of Exporter.Listener.")
			return
		}
		for _, event := range events {
			metricName := ""
			prometheusLabels := event.Labels()

			labels, present := b.mapper.getMapping(event.MetricName())
			if present {
				metricName = labels["name"]
				for label, value := range labels {
					if label != "name" {
						prometheusLabels[label] = value
					}
				}
			} else {
				eventsUnmapped.Inc()
				metricName = escapeMetricName(event.MetricName())
			}

			switch ev := event.(type) {
			case *CounterEvent:
				counter := b.Counters.Get(
					b.suffix(metricName, "counter"),
					prometheusLabels,
				)
				// We don't accept negative values for counters. Incrementing the counter with a negative number
				// will cause the exporter to panic. Instead we will warn and continue to the next event.
				if event.Value() < 0.0 {
					log.Errorf("Counter %q is: '%f' (counter must be non-negative value)", metricName, event.Value())
					continue
				}

				counter.Add(event.Value())

				eventStats.WithLabelValues("counter").Inc()

			case *GaugeEvent:
				gauge := b.Gauges.Get(
					b.suffix(metricName, "gauge"),
					prometheusLabels,
				)

				if ev.relative {
					gauge.Add(event.Value())
				} else {
					gauge.Set(event.Value())
				}

				eventStats.WithLabelValues("gauge").Inc()

			case *TimerEvent:
				summary := b.Summaries.Get(
					b.suffix(metricName, "timer"),
					prometheusLabels,
				)
				summary.Observe(event.Value())

				eventStats.WithLabelValues("timer").Inc()

			default:
				log.Errorln("Unsupported event type")
				eventStats.WithLabelValues("illegal").Inc()
			}
		}
	}
}

func NewExporter(mapper *metricMapper, addSuffix bool) *Exporter {
	return &Exporter{
		addSuffix: addSuffix,
		Counters:  NewCounterContainer(),
		Gauges:    NewGaugeContainer(),
		Summaries: NewSummaryContainer(),
		mapper:    mapper,
	}
}

type StatsDListener struct {
	conn *net.UDPConn
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

func (l *StatsDListener) Listen(e chan<- Events) {
	buf := make([]byte, 65535)
	for {
		n, _, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			log.Fatal(err)
		}
		l.handlePacket(buf[0:n], e)
	}
}

func parseDogStatsDTagsToLabels(component string) map[string]string {
	labels := map[string]string{}
	networkStats.WithLabelValues("dogstatsd_tags").Inc()
	tags := strings.Split(component, ",")
	for _, t := range tags {
		t = strings.TrimPrefix(t, "#")
		kv := strings.SplitN(t, ":", 2)

		if len(kv) < 2 || len(kv[1]) == 0 {
			networkStats.WithLabelValues("malformed_dogstatsd_tag").Inc()
			log.Errorf("Malformed or empty DogStatsD tag %s in component %s", t, component)
			continue
		}

		labels[escapeMetricName(kv[0])] = kv[1]
	}
	return labels
}

func (l *StatsDListener) handlePacket(packet []byte, e chan<- Events) {
	lines := strings.Split(string(packet), "\n")
	events := Events{}
	for _, line := range lines {
		if line == "" {
			continue
		}

		elements := strings.SplitN(line, ":", 2)
		if len(elements) < 2 || len(elements[0]) == 0 || !utf8.ValidString(line) {
			networkStats.WithLabelValues("malformed_line").Inc()
			log.Errorln("Bad line from StatsD:", line)
			continue
		}
		metric := elements[0]
		var samples []string
		if strings.Contains(elements[1], "|#") {
			// using datadog extensions, disable multi-metrics
			samples = elements[1:]
		} else {
			samples = strings.Split(elements[1], ":")
		}
		samples: for _, sample := range samples {
			components := strings.Split(sample, "|")
			samplingFactor := 1.0
			if len(components) < 2 || len(components) > 4 {
				networkStats.WithLabelValues("malformed_component").Inc()
				log.Errorln("Bad component on line:", line)
				continue
			}
			valueStr, statType := components[0], components[1]

			var relative = false
			if strings.Index(valueStr, "+") == 0 || strings.Index(valueStr, "-") == 0 {
				relative = true
			}

			value, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				log.Errorf("Bad value %s on line: %s", valueStr, line)
				networkStats.WithLabelValues("malformed_value").Inc()
				continue
			}

			labels := map[string]string{}
			if len(components) >= 3 {
				for _, component := range components[2:] {
					if len(component) == 0 {
						log.Errorln("Empty component on line: ", line)
						networkStats.WithLabelValues("malformed_component").Inc()
						continue samples
					}
				}

				for _, component := range components[2:] {
					switch component[0] {
					case '@':
						if statType != "c" {
							log.Errorln("Illegal sampling factor for non-counter metric on line", line)
							networkStats.WithLabelValues("illegal_sample_factor").Inc()
						}
						samplingFactor, err = strconv.ParseFloat(component[1:], 64)
						if err != nil {
							log.Errorf("Invalid sampling factor %s on line %s", component[1:], line)
							networkStats.WithLabelValues("invalid_sample_factor").Inc()
						}
						if samplingFactor == 0 {
							samplingFactor = 1
						}
						value /= samplingFactor
					case '#':
						labels = parseDogStatsDTagsToLabels(component)
					default:
						log.Errorf("Invalid sampling factor or tag section %s on line %s", components[2], line)
						networkStats.WithLabelValues("invalid_sample_factor").Inc()
						continue
					}
				}
			}

			event, err := buildEvent(statType, metric, value, relative, labels)
			if err != nil {
				log.Errorf("Error building event on line %s: %s", line, err)
				networkStats.WithLabelValues("illegal_event").Inc()
				continue
			}
			events = append(events, event)
			networkStats.WithLabelValues("legal").Inc()
		}
	}
	e <- events
}
