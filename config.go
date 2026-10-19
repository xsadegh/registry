package main

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"strings"

	"go.uber.org/config"
	"go.uber.org/zap"
)

//go:embed config.yaml
var defaultConfig []byte

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Log     LogConfig     `yaml:"log"`
	Storage StorageConfig `yaml:"storage"`
	Backup  BackupConfig  `yaml:"backup"`
}

type BackupConfig struct {
	S3 S3Config `yaml:"s3"`
}

func (b BackupConfig) enabled() bool {
	return b.S3.Bucket != ""
}

type ServerConfig struct {
	Addr      string `yaml:"addr"`
	PublicURL string `yaml:"public_url"`
	TLSCert   string `yaml:"tls_cert"`
	TLSKey    string `yaml:"tls_key"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type StorageConfig struct {
	Driver     string           `yaml:"driver"`
	S3         S3Config         `yaml:"s3"`
	Filesystem FilesystemConfig `yaml:"filesystem"`
}

type FilesystemConfig struct {
	Root string `yaml:"root"`
}

type S3Config struct {
	Bucket         string `yaml:"bucket"`
	Region         string `yaml:"region"`
	AccessKey      string `yaml:"access_key"`
	SecretKey      string `yaml:"secret_key"`
	Endpoint       string `yaml:"endpoint"`
	Prefix         string `yaml:"prefix"`
	ForcePathStyle bool   `yaml:"force_path_style"`
}

func loadConfig(path string) (Config, error) {
	opts := []config.YAMLOption{config.Source(bytes.NewReader(defaultConfig)), config.Expand(os.LookupEnv)}
	if path != "" {
		opts = append(opts, config.File(path))
	}
	var c Config
	y, err := config.NewYAML(opts...)
	if err != nil {
		return c, err
	}
	if err := y.Get(config.Root).Populate(&c); err != nil {
		return c, err
	}
	c.Server.PublicURL = strings.TrimSuffix(c.Server.PublicURL, "/")
	return c, c.validate()
}

func (c Config) validate() error {
	if _, err := zap.ParseAtomicLevel(c.Log.Level); err != nil {
		return err
	}
	if c.Log.Format != "json" && c.Log.Format != "console" {
		return errors.New("log.format must be json or console")
	}
	if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
		return errors.New("set both server.tls_cert and server.tls_key, or neither")
	}
	switch c.Storage.Driver {
	case "filesystem":
		if c.Storage.Filesystem.Root == "" {
			return errors.New("storage.filesystem.root is empty")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" {
			return errors.New("storage.s3.bucket is empty")
		}
		if c.Storage.S3.Region == "" && c.Storage.S3.Endpoint == "" {
			return errors.New("set storage.s3.region or storage.s3.endpoint")
		}
	default:
		return errors.New("storage.driver must be filesystem or s3")
	}
	if c.Backup.enabled() {
		b, s := c.Backup.S3, c.Storage.S3
		if b.Region == "" && b.Endpoint == "" {
			return errors.New("set backup.s3.region or backup.s3.endpoint")
		}
		if c.Storage.Driver == "s3" && b.Endpoint == s.Endpoint && b.url() == s.url() {
			return errors.New("backup.s3 must not use the same bucket and prefix as storage.s3")
		}
	}
	return nil
}

func (c Config) driverParams() map[string]any {
	if c.Storage.Driver == "filesystem" {
		return map[string]any{"rootdirectory": c.Storage.Filesystem.Root}
	}
	return c.Storage.S3.params()
}

func (s S3Config) params() map[string]any {
	params := map[string]any{"bucket": s.Bucket, "forcepathstyle": s.ForcePathStyle}
	for key, v := range map[string]string{
		"region":         s.Region,
		"regionendpoint": s.Endpoint,
		"accesskey":      s.AccessKey,
		"secretkey":      s.SecretKey,
		"rootdirectory":  s.Prefix,
	} {
		if v != "" {
			params[key] = v
		}
	}
	return params
}

func (s S3Config) url() string {
	return strings.TrimSuffix("s3://"+s.Bucket+"/"+strings.Trim(s.Prefix, "/"), "/")
}

func (c Config) location() string {
	if c.Storage.Driver == "s3" {
		return c.Storage.S3.url()
	}
	return c.Storage.Filesystem.Root
}
