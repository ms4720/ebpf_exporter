package exporter

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudflare/ebpf_exporter/config"
	"github.com/cloudflare/ebpf_exporter/decoder"
	"github.com/iovisor/gobpf/bcc"
	"github.com/prometheus/client_golang/prometheus"
)

// Namespace to use for all metrics
const prometheusNamespace = "ebpf_exporter"

// Exporter is a ebpf_exporter instance implementing prometheus.Collector
type Exporter struct {
	config   config.Config
	modules  map[string]*bcc.Module
	ksyms    map[uint64]string
	descs    map[string]map[string]*prometheus.Desc
	decoders *decoder.Set
}

// New creates a new exporter with the provided config
func New(config config.Config) *Exporter {
	return &Exporter{
		config:   config,
		modules:  map[string]*bcc.Module{},
		ksyms:    map[uint64]string{},
		descs:    map[string]map[string]*prometheus.Desc{},
		decoders: decoder.NewSet(),
	}
}

// Attach injects eBPF into kernel and attaches necessary kprobes
func (e *Exporter) Attach() error {
	for _, program := range e.config.Programs {
		if _, ok := e.modules[program.Name]; ok {
			return fmt.Errorf("multiple programs with name %q", program.Name)
		}

		module := bcc.NewModule(program.Code, []string{})
		if module == nil {
			return fmt.Errorf("error compiling module for program %q", program.Name)
		}

		for kprobeName, targetName := range program.Kprobes {
			target, err := module.LoadKprobe(targetName)
			if err != nil {
				return fmt.Errorf("failed to load target %q in program %q: %s", targetName, program.Name, err)
			}

			err = module.AttachKprobe(kprobeName, target)
			if err != nil {
				return fmt.Errorf("failed to attach kprobe %q to %q in program %q: %s", kprobeName, targetName, program.Name, err)
			}
		}

		for kretprobeName, targetName := range program.Kretprobes {
			target, err := module.LoadKprobe(targetName)
			if err != nil {
				return fmt.Errorf("failed to load target %s in program %s: %s", targetName, program.Name, err)
			}

			err = module.AttachKretprobe(kretprobeName, target)
			if err != nil {
				return fmt.Errorf("failed to attach kretprobe %s to %s in program %s: %s", kretprobeName, targetName, program.Name, err)
			}
		}

		e.modules[program.Name] = module
	}

	return nil
}

// Describe satisfies prometheus.Collector interface by sending descriptions
// for all metrics the exporter can possibly report
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	addDescs := func(programName string, name string, help string, labels []config.Label) {
		if _, ok := e.descs[programName][name]; !ok {
			labelNames := []string{}

			for _, label := range labels {
				labelNames = append(labelNames, label.Name)
			}

			e.descs[programName][name] = prometheus.NewDesc(prometheus.BuildFQName(prometheusNamespace, "", name), help, labelNames, nil)
		}

		ch <- e.descs[programName][name]
	}

	for _, program := range e.config.Programs {
		if _, ok := e.descs[program.Name]; !ok {
			e.descs[program.Name] = map[string]*prometheus.Desc{}
		}

		for _, counter := range program.Metrics.Counters {
			addDescs(program.Name, counter.Name, counter.Help, counter.Labels)
		}

		for _, histogram := range program.Metrics.Histograms {
			addDescs(program.Name, histogram.Name, histogram.Help, histogram.Labels[0:len(histogram.Labels)-1])
		}
	}
}

// Collect satisfies prometeus.Collector interface and sends all metrics
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.collectCounters(ch)
	e.collectHistograms(ch)
}

// collectCounters sends all known counters to prometheus
func (e *Exporter) collectCounters(ch chan<- prometheus.Metric) {
	for _, program := range e.config.Programs {
		for _, counter := range program.Metrics.Counters {
			tableValues, err := e.tableValues(e.modules[program.Name], counter.Table, counter.Labels)
			if err != nil {
				log.Printf("Error getting table %q values for metric %q of program %q: %s", counter.Table, counter.Name, program.Name, err)
				continue
			}

			desc := e.descs[program.Name][counter.Name]

			for _, metricValue := range tableValues {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, metricValue.value, metricValue.labels...)
			}
		}
	}
}

// collectHistograms sends all known historams to prometheus
func (e *Exporter) collectHistograms(ch chan<- prometheus.Metric) {
	for _, program := range e.config.Programs {
		for _, histogram := range program.Metrics.Histograms {
			skip := false

			histograms := map[string]histogramWithLabels{}

			tableValues, err := e.tableValues(e.modules[program.Name], histogram.Table, histogram.Labels)
			if err != nil {
				log.Printf("Error getting table %q values for metric %q of program %q: %s", histogram.Table, histogram.Name, program.Name, err)
				continue
			}

			// Taking the last label and using int as bucket delimiter, for example:
			//
			// Before:
			// * [sda, read, 1ms] -> 10
			// * [sda, read, 2ms] -> 2
			// * [sda, read, 4ms] -> 5
			//
			// After:
			// * [sda, read] -> {1ms -> 10, 2ms -> 2, 4ms -> 5}
			for _, metricValue := range tableValues {
				labels := metricValue.labels[0 : len(metricValue.labels)-1]

				key := fmt.Sprintf("%#v", labels)

				if _, ok := histograms[key]; !ok {
					histograms[key] = histogramWithLabels{
						labels:  labels,
						buckets: map[float64]uint64{},
					}
				}

				leUint, err := strconv.ParseUint(metricValue.labels[len(metricValue.labels)-1], 0, 64)
				if err != nil {
					log.Printf("Error parsing float value for bucket %#v in table %q of program %q: %s", metricValue.labels, histogram.Table, program.Name, err)
					skip = true
					break
				}

				histograms[key].buckets[float64(leUint)] = uint64(metricValue.value)
			}

			if skip {
				continue
			}

			desc := e.descs[program.Name][histogram.Name]

			for _, histogramSet := range histograms {
				buckets, count, err := transformHistogram(histogramSet.buckets, histogram)
				if err != nil {
					log.Printf("Error transforming histogram for metric %q in program %q: %s", histogram.Name, program.Name, err)
					continue
				}

				// Sum is explicitly set to zero. We only take bucket values from
				// eBPF tables, which means we lose precision and cannot calculate
				// average values from histograms anyway.
				// Lack of sum also means we cannot have +Inf bucket, only some finite
				// value bucket, eBPF programs must cap bucket values to work with this.
				ch <- prometheus.MustNewConstHistogram(desc, count, 0, buckets, histogramSet.labels...)
			}
		}
	}
}

func (e *Exporter) tableValues(module *bcc.Module, tableName string, labels []config.Label) ([]metricValue, error) {
	values := []metricValue{}

	table := bcc.NewTable(module.TableId(tableName), module)

	for entry := range table.Iter() {
		elements := strings.Fields(strings.Trim(entry.Key, "{ }"))

		if len(elements) != len(labels) {
			return nil, fmt.Errorf("key %q has %d elements, but we expect %d", entry.Key, len(elements), len(labels))
		}

		mv := metricValue{
			raw:    entry.Key,
			labels: make([]string, len(labels)),
		}

		skip := false

		for i, label := range labels {
			decoded, err := e.decoders.Decode(elements[i], label)
			if err != nil {
				if err == decoder.ErrSkipLabelSet {
					skip = true
					break
				}
				return nil, fmt.Errorf("error decoding %q for label %q: %s", elements[i], label.Name, err)
			}

			mv.labels[i] = decoded
		}

		if skip {
			continue
		}

		value, err := strconv.ParseUint(entry.Value, 0, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q for key %v cannot be parsed as uint64: %s", entry.Value, mv.labels, err)
		}

		mv.value = float64(value)

		values = append(values, mv)
	}

	return values, nil
}

func (e Exporter) exportTables() (map[string]map[string][]metricValue, error) {
	tables := map[string]map[string][]metricValue{}

	for _, program := range e.config.Programs {
		module := e.modules[program.Name]
		if module == nil {
			return nil, fmt.Errorf("module for program %q is not attached", program.Name)
		}

		if _, ok := tables[program.Name]; !ok {
			tables[program.Name] = map[string][]metricValue{}
		}

		metricTables := map[string][]config.Label{}

		for _, counter := range program.Metrics.Counters {
			if counter.Table != "" {
				metricTables[counter.Table] = counter.Labels
			}
		}

		for _, histogram := range program.Metrics.Histograms {
			if histogram.Table != "" {
				metricTables[histogram.Table] = histogram.Labels
			}
		}

		for name, labels := range metricTables {
			metricValues, err := e.tableValues(e.modules[program.Name], name, labels)
			if err != nil {
				return nil, fmt.Errorf("error getting values for table %q of program %q", name, program.Name)
			}

			tables[program.Name][name] = metricValues
		}
	}

	return tables, nil
}

// TablesHandler is a debug handler to print raw values of kernel maps
func (e *Exporter) TablesHandler(w http.ResponseWriter, r *http.Request) {
	tables, err := e.exportTables()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Header().Add("Content-type", "text/plain")
		fmt.Fprintf(w, "%s\n", err)
		return
	}

	w.Header().Add("Content-type", "text/plain")

	for program, tables := range tables {
		fmt.Fprintf(w, "## Program: %s\n\n", program)

		for name, table := range tables {
			fmt.Fprintf(w, "### Table: %s\n\n", name)

			fmt.Fprintf(w, "```\n")
			for _, row := range table {
				fmt.Fprintf(w, "%s (%v) -> %f\n", row.raw, row.labels, row.value)
			}
			fmt.Fprintf(w, "```\n\n")
		}
	}
}

// metricValue is a row in a kernel map
type metricValue struct {
	// raw is a raw key value provided by kernel
	raw string
	// labels are decoded from the raw key
	labels []string
	// value is the kernel map value
	value float64
}
