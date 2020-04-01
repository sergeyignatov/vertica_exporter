package main

import (
	"context"
	"sync"

	"encoding/json"
	"fmt"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo"
	log "github.com/labstack/gommon/log"
	"github.com/namsral/flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/vertica/vertica-sql-go"
	vlogger "github.com/vertica/vertica-sql-go/logger"
	"net/http"
	"os"
	"strings"
	"time"
)

type Server struct {
	Listen                      string
	DBLogin, DBPassword, DBHost string
	lastPingError               error
	pingPeriod                  time.Duration
	timeout                     time.Duration
	db                          *sqlx.DB
	settings                    Settings
	l                           sync.RWMutex
}
type Query struct {
	Query      interface{} `json:"query"`
	Name       string      `json:"name"`
	Labels     []string
	ValueField string `json:"value_field_name"`
}

func (q Query) String() (s string) {

	switch t := q.Query.(type) {
	case []interface{}:
		tmp := make([]string, len(t))
		for i, ss := range t {
			tmp[i] = ss.(string)
		}
		s = strings.Join(tmp, " ")
	case string:
		s = t
	}
	return
}
func (s *Server) ping() (err error) {

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)

	defer cancel()
	errChan := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.dbGETContext(ctx, "select 1 as test")
		errChan <- err
	}()
	go func() {
		wg.Wait()
		close(errChan)
	}()
	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

}

type Settings struct {
	DBHost  string `json:"dbhost"`
	DBUser  string `json:"dbuser"`
	DBPass  string `json:"dbpass"`
	Queries []Query
}

var (
	namespace = "vertica"
)

func customHTTPErrorHandler(err error, c echo.Context) {
	code := http.StatusInternalServerError
	if he, ok := err.(*echo.HTTPError); ok {
		code = he.Code
	}
	c.Logger().Error(err)
	if err := c.JSON(code, echo.HTTPError{Code: code, Message: err.Error()}); err != nil {
		c.Logger().Error(err)
	}
}
func (s *Server) updatePingStatus(err error) {
	s.l.Lock()
	defer s.l.Unlock()
	s.lastPingError = err
}
func (s *Server) pingStatus() error {
	s.l.RLock()
	defer s.l.RUnlock()
	return s.lastPingError
}
func (s *Server) Collect(ch chan<- prometheus.Metric) {
	if s.pingStatus() != nil {
		return
	}
	if err := s.collect(ch); err != nil {
		log.Error(err)
	}
}

func (s *Server) dbGETContext(ctx context.Context, q string) (t []map[string]interface{}, err error) {
	rows, err := s.db.QueryxContext(ctx, q)

	if err != nil {
		return
	}
	defer rows.Close()
	t = []map[string]interface{}{}
	for rows.Next() {
		result := make(map[string]interface{})
		if err = rows.MapScan(result); err != nil {
			return
		}
		t = append(t, result)
	}
	return
}
func (s *Server) dbGET(q string) (t []map[string]interface{}, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	return s.dbGETContext(ctx, q)
}

func (s *Server) convertValue(i interface{}) (v float64, err error) {
	switch t := i.(type) {
	case int:
		v = float64(t)
	case float64:
		v = t
	default:
		err = fmt.Errorf("unable convert %v into float64", i)
	}
	return
}
func (s *Server) collect(ch chan<- prometheus.Metric) (err error) {

	var val float64
	for _, q := range s.settings.Queries {
		rows, err := s.dbGET(q.String())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}

		labels := make([]string, 0)
		for k := range rows[0] {
			if strings.ToLower(k) != q.ValueField {
				labels = append(labels, k)
			}
		}
		if len(labels) == 0 && len(rows[0]) > 1 {
			log.Error("labels is empty, ", q.ValueField)
			continue
		}
		for _, r := range rows {
			values := make([]string, len(labels))
			for i, k := range labels {
				values[i] = fmt.Sprintf("%v", r[k])
			}
			newMetric := prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      q.Name,
				Help:      q.Name,
			}, labels).WithLabelValues(values...)
			if val, err = s.convertValue(r[q.ValueField]); err != nil {
				return err
			}

			newMetric.Set(val)

			newMetric.Collect(ch)

		}

	}

	return
}
func (s *Server) Describe(ch chan<- *prometheus.Desc) {
	metricCh := make(chan prometheus.Metric, 2)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	s.Collect(metricCh)
	close(metricCh)
	<-doneCh
}
func (s *Server) shutdown() {
	log.Info("shutdown")
	if s.db != nil {
		s.db.Close()
	}
}
func (s *Server) dsn() string {
	return fmt.Sprintf("vertica://%s:%s@%s/dbadmin", s.DBLogin, s.DBPassword, s.DBHost)
}

func (s *Server) initDB() {
	var err error
	s.updatePingStatus(fmt.Errorf("not connected"))
	s.db, err = sqlx.Open("vertica", s.dsn())
	if err != nil {
		log.Error(err)
	}
}
func (s *Server) reconnect() {
	s.db.Close()
	s.db, _ = sqlx.Open("vertica", s.dsn())
}
func (s *Server) pingConn() {
	var err error

	for {
		if err = s.ping(); err != nil {
			s.updatePingStatus(err)
			s.reconnect()
		} else {
			s.updatePingStatus(nil)
		}
		time.Sleep(s.pingPeriod)
	}
}
func (s *Server) Start() (err error) {
	e := echo.New()
	e.HTTPErrorHandler = customHTTPErrorHandler
	e.HideBanner = true
	e.HidePort = true
	e.Debug = false
	if err := prometheus.Register(s); err != nil {
		log.Error(err)
	}

	go s.pingConn()
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	e.GET("/ping", func(c echo.Context) (err error) {

		if err = s.pingStatus(); err != nil {
			return err
		}
		return c.String(200, "OK")

	})
	return e.Start(s.Listen)
}

func run() (err error) {
	listen := flag.String("listen", ":9401", "listen on")
	dbLogin := flag.String("vertica_login", "dbadmin", "vertica username")
	dbPassword := flag.String("vertica_password", "dbadmin", "vertica password")
	dbHost := flag.String("vertica_host", "127.0.0.1:5433", "vertica host")
	pingPeriodPtr := flag.String("ping-period", "10s", "database ping period")
	timeoutPtr := flag.String("timeout", "5s", "database timeout")
	settingsFile := flag.String("settings", "./settings.json", "files with settings")

	flag.Parse()
	vlogger.SetLogLevel(vlogger.NONE)
	settings := Settings{}
	if fd, err := os.Open(*settingsFile); err == nil {
		dec := json.NewDecoder(fd)
		if err = dec.Decode(&settings); err != nil {
			return err
		}
	} else {
		log.Error(err)
	}
	pingPeriod, err := time.ParseDuration(*pingPeriodPtr)
	if err != nil {
		return
	}
	timeout, err := time.ParseDuration(*timeoutPtr)
	if err != nil {
		return
	}
	s := Server{
		Listen:     *listen,
		DBLogin:    *dbLogin,
		DBPassword: *dbPassword,
		DBHost:     *dbHost,
		pingPeriod: pingPeriod,
		timeout:    timeout,
		settings:   settings,
	}

	s.initDB()

	defer s.shutdown()
	return s.Start()
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
