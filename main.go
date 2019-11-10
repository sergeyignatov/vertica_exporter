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
)

type Server struct {
	Listen                      string
	DBLogin, DBPassword, DBHost string
	lastPingTime                time.Time
	lastPingError               error
	db                          *sqlx.DB
}
type Query struct {
	q          string
	name       string
	labels     []string
	valueField string
}

var (
	namespace = "vertica"
	queries   = []Query{
		{
			q: `SELECT /*+ label(sessions_by_query_type) */ query_type, COUNT(*) - 1 AS query_avg_count 
				FROM (  SELECT UPPER(regexp_substr(current_statement::char(8),'^[^\\s]+')::CHAR(8 )) AS query_type FROM sessions UNION ALL 
						SELECT 'INSERT' UNION ALL SELECT 'MERGE' UNION ALL SELECT 'SELECT' UNION ALL SELECT 'DELETE' UNION ALL SELECT 'COPY' UNION ALL SELECT 'ALTER' UNION ALL SELECT 'TRUNCATE' UNION ALL SELECT 'CREATE' ) interval_query WHERE query_type NOT IN ('COMMIT','SET') AND regexp_like(query_type,'^[A-Z]+$') GROUP BY query_type`,
			labels:     []string{"query_type"},
			valueField: "query_avg_count",
			name:       "sessions_by_query_type",
		},
		{

			q:          `select  /*+ label(catalog_size_mb) */   a.node_name as node_name,  floor((((sum((a.total_memory - a.free_memory)) / 1024::numeric(18,0)) / 1024::numeric(18,0)) )) AS catalog_size_mb FROM (v_internal.dc_allocation_pool_statistics a JOIN (SELECT dc_allocation_pool_statistics.node_name, date_trunc('SECOND'::varchar(6), max(dc_allocation_pool_statistics.time)) AS time FROM v_internal.dc_allocation_pool_statistics GROUP BY dc_allocation_pool_statistics.node_name) b ON (((a.node_name = b.node_name) AND (date_trunc('SECOND'::varchar(6), a.time) = b.time)))) GROUP BY a.node_name, b.time ORDER BY floor((((sum((a.total_memory - a.free_memory)) / 1024::numeric(18,0)) / 1024::numeric(18,0)) )) DESC`,
			labels:     []string{"node_name"},
			valueField: "catalog_size_mb",
			name:       "catalog_size_mb",
		},
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	log.Info("collect")
	var val float64
	for _, q := range queries {
		rows, err := s.dbGET(q.q)
		if err != nil {
			return err
		}

		for _, r := range rows {
			values := make([]string, len(q.labels))
			for i, k := range q.labels {
				values[i] = fmt.Sprintf("%v", r[k])
			}
			newMetric := prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      q.name,
				Help:      q.name,
			}, q.labels).WithLabelValues(values...)
			if val, err = s.convertValue(r[q.valueField]); err != nil {
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
	prometheus.MustRegister(s)
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
	flag.Parse()

	s := Server{
		Listen:     *listen,
		DBLogin:    *dbLogin,
		DBPassword: *dbPassword,
		DBHost:     *dbHost,
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
