// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package udp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/rcrowley/go-metrics"

	input "github.com/elastic/beats/v7/filebeat/input/v2"
	stateless "github.com/elastic/beats/v7/filebeat/input/v2/input-stateless"
	"github.com/elastic/beats/v7/filebeat/inputsource"
	"github.com/elastic/beats/v7/filebeat/inputsource/udp"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/feature"
	"github.com/elastic/beats/v7/libbeat/monitoring/inputmon"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/elastic-agent-libs/monitoring"
	"github.com/elastic/elastic-agent-libs/monitoring/adapter"
	"github.com/elastic/go-concert/ctxtool"
)

func Plugin() input.Plugin {
	return input.Plugin{
		Name:       "udp",
		Stability:  feature.Stable,
		Deprecated: false,
		Info:       "udp packet server",
		Manager:    stateless.NewInputManager(configure),
	}
}

func configure(cfg *conf.C) (stateless.Input, error) {
	config := defaultConfig()
	if err := cfg.Unpack(&config); err != nil {
		return nil, err
	}

	return newServer(config)
}

func defaultConfig() config {
	return config{
		Config: udp.Config{
			MaxMessageSize: 10 * humanize.KiByte,
			Host:           "localhost:8080",
			Timeout:        time.Minute * 5,
		},
	}
}

type server struct {
	udp.Server
	config
}

type config struct {
	udp.Config `config:",inline"`
}

func newServer(config config) (*server, error) {
	return &server{config: config}, nil
}

func (s *server) Name() string { return "udp" }

func (s *server) Test(_ input.TestContext) error {
	l, err := net.Listen("udp", s.config.Config.Host)
	if err != nil {
		return err
	}
	return l.Close()
}

func (s *server) Run(ctx input.Context, publisher stateless.Publisher) error {
	log := ctx.Logger.With("host", s.config.Config.Host)

	log.Info("starting udp socket input")
	defer log.Info("udp input stopped")

	const pollInterval = time.Minute
	metrics := newInputMetrics(ctx.ID, s.config.Host, uint64(s.config.ReadBuffer), pollInterval, log)
	defer metrics.close()

	server := udp.New(&s.config.Config, func(data []byte, metadata inputsource.NetworkMetadata) {
		evt := beat.Event{
			Timestamp: time.Now(),
			Meta: mapstr.M{
				"truncated": metadata.Truncated,
			},
			Fields: mapstr.M{
				"message": string(data),
			},
		}
		if metadata.RemoteAddr != nil {
			evt.Fields["log"] = mapstr.M{
				"source": mapstr.M{
					"address": metadata.RemoteAddr.String(),
				},
			}
		}

		publisher.Publish(evt)

		// This must be called after publisher.Publish to measure
		// the processing time metric.
		metrics.log(data, evt.Timestamp)
	})

	log.Debug("udp input initialized")

	err := server.Run(ctxtool.FromCanceller(ctx.Cancelation))
	// Ignore error from 'Run' in case shutdown was signaled.
	if ctxerr := ctx.Cancelation.Err(); ctxerr != nil {
		err = ctxerr
	}
	return err
}

// inputMetrics handles the input's metric reporting.
type inputMetrics struct {
	unregister func()
	done       chan struct{}

	lastPacket time.Time

	device         *monitoring.String // name of the device being monitored
	packets        *monitoring.Uint   // number of packets processed
	bytes          *monitoring.Uint   // number of bytes processed
	bufferLen      *monitoring.Uint   // configured read buffer length
	rxQueue        *monitoring.Uint   // value of the rx_queue field from /proc/net/udp (only on linux systems)
	drops          *monitoring.Uint   // number of udp drops noted in /proc/net/udp
	arrivalPeriod  metrics.Sample     // histogram of the elapsed time between packet arrivals
	processingTime metrics.Sample     // histogram of the elapsed time between packet receipt and publication
}

// newInputMetrics returns an input metric for the UDP processor. If id is empty
// a nil inputMetric is returned.
func newInputMetrics(id, device string, buflen uint64, poll time.Duration, log *logp.Logger) *inputMetrics {
	if id == "" {
		return nil
	}
	reg, unreg := inputmon.NewInputRegistry("udp", id, nil)
	out := &inputMetrics{
		unregister:     unreg,
		bufferLen:      monitoring.NewUint(reg, "udp_read_buffer_length_gauge"),
		device:         monitoring.NewString(reg, "device"),
		packets:        monitoring.NewUint(reg, "received_events_total"),
		bytes:          monitoring.NewUint(reg, "received_bytes_total"),
		rxQueue:        monitoring.NewUint(reg, "receive_queue_length"),
		drops:          monitoring.NewUint(reg, "system_packet_drops"),
		arrivalPeriod:  metrics.NewUniformSample(1024),
		processingTime: metrics.NewUniformSample(1024),
	}
	_ = adapter.NewGoMetrics(reg, "arrival_period", adapter.Accept).
		Register("histogram", metrics.NewHistogram(out.arrivalPeriod))
	_ = adapter.NewGoMetrics(reg, "processing_time", adapter.Accept).
		Register("histogram", metrics.NewHistogram(out.processingTime))

	out.device.Set(device)
	out.bufferLen.Set(buflen)

	if poll > 0 && runtime.GOOS == "linux" {
		host, port, ok := strings.Cut(device, ":")
		if !ok {
			log.Warnf("failed to get address for %s: no port separator", device)
			return out
		}
		ip, err := net.LookupIP(host)
		if err != nil {
			log.Warnf("failed to get address for %s: %v", device, err)
			return out
		}
		p, err := strconv.ParseInt(port, 10, 16)
		if err != nil {
			log.Warnf("failed to get port for %s: %v", device, err)
			return out
		}
		ph := strconv.FormatInt(p, 16)
		addr := make([]string, 0, len(ip))
		for _, p := range ip {
			p4 := p.To4()
			if len(p4) != net.IPv4len {
				continue
			}
			addr = append(addr, fmt.Sprintf("%X:%s", binary.LittleEndian.Uint32(p4), ph))
		}
		out.done = make(chan struct{})
		go out.poll(addr, poll, log)
	}

	return out
}

// log logs metric for the given packet.
func (m *inputMetrics) log(data []byte, timestamp time.Time) {
	if m == nil {
		return
	}
	m.processingTime.Update(time.Since(timestamp).Nanoseconds())
	m.packets.Add(1)
	m.bytes.Add(uint64(len(data)))
	if !m.lastPacket.IsZero() {
		m.arrivalPeriod.Update(timestamp.Sub(m.lastPacket).Nanoseconds())
	}
	m.lastPacket = timestamp
}

// poll periodically gets UDP buffer and packet drops stats from the OS.
func (m *inputMetrics) poll(addr []string, each time.Duration, log *logp.Logger) {
	t := time.NewTicker(each)
	for {
		select {
		case <-t.C:
			rx, drops, err := procNetUDP("/proc/net/udp", addr)
			if err != nil {
				log.Warnf("failed to get udp stats from /proc: %v", err)
				continue
			}
			m.rxQueue.Set(uint64(rx))
			m.drops.Set(uint64(drops))
		case <-m.done:
			t.Stop()
			return
		}
	}
}

// procNetUDP returns the rx_queue and drops field of the UDP socket table
// for the socket on the provided address formatted in hex, xxxxxxxx:xxxx.
// This function is only useful on linux due to its dependence on the /proc
// filesystem, but is kept in this file for simplicity.
func procNetUDP(path string, addr []string) (rx, drops int64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("%s entry not found for %s (no line)", path, addr)
	}
	for _, l := range lines[1:] {
		f := bytes.Fields(l)
		if len(f) > 12 && contains(f[1], addr) {
			_, r, ok := bytes.Cut(f[4], []byte(":"))
			if !ok {
				return 0, 0, errors.New("no rx_queue field " + string(f[4]))
			}
			rx, err = strconv.ParseInt(string(r), 16, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("failed to parse rx_queue: %w", err)
			}
			drops, err = strconv.ParseInt(string(f[12]), 16, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("failed to parse drops: %w", err)
			}
			return rx, drops, nil
		}
	}
	return 0, 0, fmt.Errorf("%s entry not found for %s", path, addr)
}

func contains(b []byte, addr []string) bool {
	for _, a := range addr {
		if strings.EqualFold(string(b), a) {
			return true
		}
	}
	return false
}

func (m *inputMetrics) close() {
	if m == nil {
		return
	}
	if m.done != nil {
		// Shut down poller and wait until done before unregistering metrics.
		m.done <- struct{}{}
	}
	m.unregister()
}
