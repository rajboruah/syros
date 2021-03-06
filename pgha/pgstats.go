package main

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"fmt"
	log "github.com/Sirupsen/logrus"
	consul "github.com/hashicorp/consul/api"
	"github.com/pkg/errors"
	"github.com/robfig/cron"
)

type PGStats struct {
	db           *sql.DB
	consulKey    string
	consulClient *consul.Client
	cron         *cron.Cron
	config       *Config
	stopChan     chan struct{}
}

type ReplicationStats struct {
	Host      string    `json:"host"`
	Role      string    `json:"role"`
	Xlog      uint64    `json:"xlog"`
	Offset    uint64    `json:"offset"`
	Timestamp time.Time `json:"timestamp"`
}

func NewPGStats(config *Config, c *cron.Cron) (*PGStats, error) {
	db, err := sql.Open("postgres", config.PostgresURI)
	if err != nil {
		return nil, errors.Wrap(err, "Postgres init failed")
	}
	db.SetMaxOpenConns(1)

	err = db.Ping()
	if err != nil {
		return nil, errors.Wrap(err, "Postgres ping failed")
	}

	key := config.ConsulKV + "/replication/stats"
	cfg := consul.DefaultConfig()
	cfg.Address = config.ConsulURI
	client, err := consul.NewClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "Consul client init failed")
	}

	pg := &PGStats{
		db:           db,
		consulKey:    key,
		consulClient: client,
		cron:         c,
		config:       config,
		stopChan:     make(chan struct{}, 1),
	}
	return pg, nil
}

func (pg *PGStats) GetReplicationStats() (ReplicationStats, error) {
	stats := ReplicationStats{
		Host:      pg.config.Hostname,
		Timestamp: time.Now().UTC(),
	}
	var isInRecovery bool
	err := pg.db.QueryRow("SELECT pg_is_in_recovery()").Scan(&isInRecovery)
	if err != nil {
		return stats, errors.Wrap(err, "Query pg_is_in_recovery failed")
	}

	var xlogLocation string
	if isInRecovery {
		stats.Role = "slave"
		err := pg.db.QueryRow("SELECT pg_last_xlog_receive_location()").Scan(&xlogLocation)
		if err != nil {
			return stats, errors.Wrap(err, "Query pg_last_xlog_receive_location failed")
		}
	} else {
		stats.Role = "master"
		err := pg.db.QueryRow("SELECT pg_current_xlog_location()").Scan(&xlogLocation)
		if err != nil {
			return stats, errors.Wrap(err, "Query pg_current_xlog_location failed")
		}
	}

	xlog, err := strconv.ParseUint(strings.Split(xlogLocation, "/")[0], 16, 32)
	if err != nil {
		return stats, errors.Wrap(err, "Parse xlog failed")
	}
	stats.Xlog = xlog

	offset, err := strconv.ParseUint(strings.Split(xlogLocation, "/")[1], 16, 32)
	if err != nil {
		return stats, errors.Wrap(err, "Parse xlog offset failed")
	}
	stats.Offset = offset

	return stats, nil
}

func (pg *PGStats) SaveReplicationStats(stats ReplicationStats) error {

	data, err := json.Marshal(stats)
	if err != nil {
		return errors.Wrap(err, "Replication stats json marshal failed")
	}

	kv := new(consul.KVPair)
	kv.Key = pg.consulKey + "/" + pg.config.Hostname
	kv.Value = data

	_, err = pg.consulClient.KV().Put(kv, nil)
	if err != nil {
		return errors.Wrap(err, "Replication stats KV Put failed")
	}

	return nil
}

func (pg *PGStats) Start() {
	pg.cron.AddFunc(fmt.Sprintf("*/%d * * * * *", pg.config.PostgresCheck), func() {
		stats, err := pg.GetReplicationStats()
		if err != nil {
			log.Warnf("PGStats GetReplicationStats failed %s", err.Error())
		} else {
			err = pg.SaveReplicationStats(stats)
			if err != nil {
				log.Warnf("PGStats SaveReplicationStats failed %s", err.Error())
			}
		}
	})
}
