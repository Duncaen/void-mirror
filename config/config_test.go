package config

import "testing"

func TestLoad(t *testing.T) {
  var c Config
  if err := c.Load("fixtures/config.hcl"); err != nil {
    t.Fatal(err)
  }
}
