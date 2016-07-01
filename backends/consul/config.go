package consul

import (
	"strings"

	"github.com/HeavyHorst/remco/backends"
	"github.com/HeavyHorst/remco/template"
	"github.com/cloudflare/cfssl/log"
	"github.com/kelseyhightower/confd/backends/consul"
)

type Config struct {
	Nodes  []string
	Scheme string
	Cert   string
	Key    string
	CaCert string
	template.StoreConfig
}

func (c *Config) Connect() (backends.StoreClient, error) {
	log.Info("Backend nodes set to " + strings.Join(c.Nodes, ", "))
	client, err := consul.New(c.Nodes, c.Scheme, c.Cert, c.Key, c.CaCert)
	if err != nil {
		return nil, err
	}
	c.StoreConfig.StoreClient = client
	return client, nil
}