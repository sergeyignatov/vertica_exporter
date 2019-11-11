package main

import (
	"context"

	"fmt"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo"
	log "github.com/labstack/gommon/log"
	"github.com/namsral/flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/vertica/vertica-sql-go"
	"net/http"
	"time"
	"os"
	"strings"
	"encoding/json"
)

type Server struct {
	Listen                      string
	DBLogin, DBPassword, DBHost string
	lastPingTime                time.Time
	lastPingError               error
	db                          *sqlx.DB
	settings Settings
}
type Query struct {
	Query           interface{} `json:"query"`
	Name       string `json:"name"`
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
type Settings struct {
	DBHost string `json:"dbhost"`
	DBUser string `json:"dbuser"`
	DBPass string `json:"dbpass"`
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
	c.JSON(code, echo.HTTPError{Code: code, Message: err.Error()})
}

func (s *Server) Collect(ch chan<- prometheus.Metric) {
	if err := s.collect(ch); err != nil {
		log.Error(err)
	}
}

func (s *Server) dbGET(q string) (t []map[string]interface{}, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
func (s *Server) initDB() {
	var err error
	s.db, err = sqlx.Open("vertica", fmt.Sprintf("vertica://%s:%s@%s/dbadmin", s.DBLogin, s.DBPassword, s.DBHost))
	if err != nil {
		log.Error(err)
	}
}
func (s *Server) Start() (err error) {
	e := echo.New()
	e.HTTPErrorHandler = customHTTPErrorHandler
	done := make(chan error)
	go func() {
		done <- prometheus.Register(s)
	}()
	select {
		case err := <-done:
				if err != nil {
					return err
				}
		case <- time.After(10* time.Second):
				return fmt.Errorf("inialize timeout, check vertica user permissions")
	}

	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
	e.GET("/ping", func(c echo.Context) (err error) {

		if time.Now().Sub(s.lastPingTime) < time.Minute {
			if s.lastPingError != nil {
				return s.lastPingError
			}
			return c.String(200, "OK")
		}

		defer func() {
			if err != nil {
				s.lastPingError = err
			}
			s.lastPingTime = time.Now()
		}()

		if err = s.db.PingContext(c.Request().Context()); err != nil {
			return
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
	settingsFile := flag.String("settings", "./settings.json", "files with settings")
	flag.Parse()
	settings := Settings{}
	if fd, err := os.Open(*settingsFile);err == nil {
		dec := json.NewDecoder(fd)
		if err = dec.Decode(&settings);err != nil {
			return err
		}
	}else {
		log.Error(err)
	}

	s := Server{
		Listen:     *listen,
		DBLogin:    *dbLogin,
		DBPassword: *dbPassword,
		DBHost:     *dbHost,
		settings: settings,
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
