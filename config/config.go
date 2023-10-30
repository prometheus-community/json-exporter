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

package config

import (
	"os"

	pconfig "github.com/prometheus/common/config"
	"gopkg.in/yaml.v2"
)

// Metric contains values that define a metric
type Metric struct {
	Name           string
	Engine         EngineType
	Path           string
	Labels         map[string]string
	Type           ScrapeType
	ValueType      ValueType
	EpochTimestamp string
	Help           string
	Values         map[string]string
}

type ScrapeType string

const (
	ValueScrape  ScrapeType = "value" // default
	ObjectScrape ScrapeType = "object"
)

type ValueType string

const (
	ValueTypeGauge   ValueType = "gauge"
	ValueTypeCounter ValueType = "counter"
	ValueTypeUntyped ValueType = "untyped" // default
)

type EngineType string

const (
	EngineTypeJSONPath EngineType = "jsonpath" // default
	EngineTypeCEL      EngineType = "cel"
)

// Config contains multiple modules.
type Config struct {
	Modules map[string]Module `yaml:"modules"`
}

// Module contains metrics and headers defining a configuration
type Module struct {
	Headers          map[string]string        `yaml:"headers,omitempty"`
	Metrics          []Metric                 `yaml:"metrics"`
	HTTPClientConfig pconfig.HTTPClientConfig `yaml:"http_client_config,omitempty"`
	Body             Body                     `yaml:"body,omitempty"`
	ValidStatusCodes []int                    `yaml:"valid_status_codes,omitempty"`
}

type Body struct {
	Content    string `yaml:"content"`
	Templatize bool   `yaml:"templatize,omitempty"`
}

func LoadConfig(configPath string) (Config, error) {
	var config Config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return config, err
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, err
	}

	// Complete Defaults
	for _, module := range config.Modules {
		for i := 0; i < len(module.Metrics); i++ {
			if module.Metrics[i].Type == "" {
				module.Metrics[i].Type = ValueScrape
			}
			if module.Metrics[i].Help == "" {
				module.Metrics[i].Help = module.Metrics[i].Name
			}
			if module.Metrics[i].ValueType == "" {
				module.Metrics[i].ValueType = ValueTypeUntyped
			}
			if module.Metrics[i].Engine == "" {
				module.Metrics[i].Engine = EngineTypeJSONPath
			}
		}
	}

	return config, nil
}
