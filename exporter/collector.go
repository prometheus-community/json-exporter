// Copyright 2020 The Prometheus Authors
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

package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/prometheus-community/json_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	structpb "google.golang.org/protobuf/types/known/structpb"
	"k8s.io/client-go/util/jsonpath"
)

type JSONMetricCollector struct {
	JSONMetrics []JSONMetric
	Data        []byte
	Logger      *slog.Logger
}

type JSONMetric struct {
	Desc                   *prometheus.Desc
	Type                   config.ScrapeType
	EngineType             config.EngineType
	KeyJSONPath            string
	ValueJSONPath          string
	LabelsJSONPaths        []string
	ValueType              prometheus.ValueType
	EpochTimestampJSONPath string
}

func (mc JSONMetricCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range mc.JSONMetrics {
		ch <- m.Desc
	}
}

func (mc JSONMetricCollector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range mc.JSONMetrics {
		switch m.Type {
		case config.ValueScrape:
			mc.Logger.Debug("Extracting value for metric", "path", m.KeyJSONPath, "metric", m.Desc)
			value, err := extractValue(mc.Logger, m.EngineType, mc.Data, m.KeyJSONPath, false)
			if err != nil {
				mc.Logger.Error("Failed to extract value for metric", "path", m.KeyJSONPath, "err", err, "metric", m.Desc)
				continue
			}

			if floatValue, err := SanitizeValue(value); err == nil {
				metric := prometheus.MustNewConstMetric(
					m.Desc,
					m.ValueType,
					floatValue,
					extractLabels(mc.Logger, m.EngineType, mc.Data, m.LabelsJSONPaths)...,
				)
				ch <- timestampMetric(mc.Logger, m, mc.Data, metric)
			} else {
				mc.Logger.Error("Failed to convert extracted value to float64", "path", m.KeyJSONPath, "value", value, "err", err, "metric", m.Desc)
				continue
			}

		case config.ObjectScrape:
			mc.Logger.Debug("Extracting object for metric", "path", m.KeyJSONPath, "metric", m.Desc)
			values, err := extractValue(mc.Logger, m.EngineType, mc.Data, m.KeyJSONPath, true)
			if err != nil {
				mc.Logger.Error("Failed to extract json objects for metric", "err", err, "metric", m.Desc)
				continue
			}

			var jsonData []interface{}
			if err := json.Unmarshal([]byte(values), &jsonData); err == nil {
				for _, data := range jsonData {
					jdata, err := json.Marshal(data)
					if err != nil {
						mc.Logger.Error("Failed to marshal data to json", "path", m.ValueJSONPath, "err", err, "metric", m.Desc, "data", data)
						continue
					}
					value, err := extractValue(mc.Logger, m.EngineType, jdata, m.ValueJSONPath, false)
					if err != nil {
						mc.Logger.Error("Failed to extract value for metric", "path", m.ValueJSONPath, "err", err, "metric", m.Desc)
						continue
					}

					if floatValue, err := SanitizeValue(value); err == nil {
						metric := prometheus.MustNewConstMetric(
							m.Desc,
							m.ValueType,
							floatValue,
							extractLabels(mc.Logger, m.EngineType, jdata, m.LabelsJSONPaths)...,
						)
						ch <- timestampMetric(mc.Logger, m, jdata, metric)
					} else {
						mc.Logger.Error("Failed to convert extracted value to float64", "path", m.ValueJSONPath, "value", value, "err", err, "metric", m.Desc)
						continue
					}
				}
			} else {
				mc.Logger.Error("Failed to convert extracted objects to json", "value", values, "err", err, "metric", m.Desc)
				continue
			}
		default:
			mc.Logger.Error("Unknown scrape config type", "type", m.Type, "metric", m.Desc)
			continue
		}
	}
}

func extractValue(logger *slog.Logger, engine config.EngineType, data []byte, path string, enableJSONOutput bool) (string, error) {
	switch engine {
	case config.EngineTypeJSONPath:
		return extractValueJSONPath(logger, data, path, enableJSONOutput)
	case config.EngineTypeCEL:
		return extractValueCEL(logger, data, path, enableJSONOutput)
	default:
		return "", fmt.Errorf("Unknown engine type: %s", engine)
	}
}

// Returns the last matching value at the given json path
func extractValueJSONPath(logger *slog.Logger, data []byte, path string, enableJSONOutput bool) (string, error) {
	var jsonData interface{}
	buf := new(bytes.Buffer)

	j := jsonpath.New("jp")
	if enableJSONOutput {
		j.EnableJSONOutput(true)
	}

	if err := json.Unmarshal(data, &jsonData); err != nil {
		logger.Error("Failed to unmarshal data to json", "err", err, "data", data)
		return "", err
	}

	if err := j.Parse(path); err != nil {
		logger.Error("Failed to parse jsonpath", "err", err, "path", path, "data", data)
		return "", err
	}

	if err := j.Execute(buf, jsonData); err != nil {
		logger.Error("Failed to execute jsonpath", "err", err, "path", path, "data", data)
		return "", err
	}

	// Since we are finally going to extract only float64, unquote if necessary
	if res, err := jsonpath.UnquoteExtend(buf.String()); err == nil {
		return res, nil
	}

	return buf.String(), nil
}

// Returns the last matching value at the given json path
func extractValueCEL(logger *slog.Logger, data []byte, expression string, enableJSONOutput bool) (string, error) {

	var jsonData map[string]any

	err := json.Unmarshal(data, &jsonData)
	if err != nil {
		logger.Error("Failed to unmarshal data to json", "err", err, "data", data)
		return "", err
	}

	inputVars := make([]cel.EnvOption, 0, len(jsonData))
	for k := range jsonData {
		inputVars = append(inputVars, cel.Variable(k, cel.DynType))
	}

	env, err := cel.NewEnv(inputVars...)

	if err != nil {
		logger.Error("Failed to set up CEL environment", "err", err, "data", data)
		return "", err
	}

	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		logger.Error("CEL type-check error", "err", issues.String(), "expression", expression)
		return "", err
	}
	prg, err := env.Program(ast)
	if err != nil {
		logger.Error("CEL program construction error", "err", err)
		return "", err
	}

	out, _, err := prg.Eval(jsonData)
	if err != nil {
		logger.Error("Failed to evaluate cel query", "err", err, "expression", expression, "data", jsonData)
		return "", err
	}

	// Since we are finally going to extract only float64, unquote if necessary

	//res, err := jsonpath.UnquoteExtend(fmt.Sprintf("%g", out))
	//if err == nil {
	//	level.Error(logger).Log("msg","Triggered")
	//	return res, nil
	//}
	logger.Error("Triggered later", "val", out)
	if enableJSONOutput {
		res, err := valueToJSON(out)
		if err != nil {
			return "", err
		}
		return res, nil
	}

	return fmt.Sprintf("%v", out), nil
}

// Returns the list of labels created from the list of provided json paths
func extractLabels(logger *slog.Logger, engine config.EngineType, data []byte, paths []string) []string {
	labels := make([]string, len(paths))
	for i, path := range paths {
		if result, err := extractValue(logger, engine, data, path, false); err == nil {
			labels[i] = result
		} else {
			logger.Error("Failed to extract label value", "err", err, "path", path, "data", data)
		}
	}
	return labels
}

func timestampMetric(logger *slog.Logger, m JSONMetric, data []byte, pm prometheus.Metric) prometheus.Metric {
	if m.EpochTimestampJSONPath == "" {
		return pm
	}
	ts, err := extractValue(logger, m.EngineType, data, m.EpochTimestampJSONPath, false)
	if err != nil {
		logger.Error("Failed to extract timestamp for metric", "path", m.KeyJSONPath, "err", err, "metric", m.Desc)
		return pm
	}
	epochTime, err := SanitizeIntValue(ts)
	if err != nil {
		logger.Error("Failed to parse timestamp for metric", "path", m.KeyJSONPath, "err", err, "metric", m.Desc)
		return pm
	}
	timestamp := time.UnixMilli(epochTime)
	return prometheus.NewMetricWithTimestamp(timestamp, pm)
}

// valueToJSON converts the CEL type to a protobuf JSON representation and
// marshals the result to a string.
func valueToJSON(val ref.Val) (string, error) {
	v, err := val.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		return "", err
	}
	marshaller := protojson.MarshalOptions{Indent: "    "}
	bytes, err := marshaller.Marshal(v.(proto.Message))
	if err != nil {
		return "", err
	}
	return string(bytes), err
}
