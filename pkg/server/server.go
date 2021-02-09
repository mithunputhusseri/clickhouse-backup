package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/AlexAkulov/clickhouse-backup/config"
	"github.com/AlexAkulov/clickhouse-backup/pkg/backup"
	"github.com/apex/log"

	"github.com/google/shlex"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli"
	yaml "gopkg.in/yaml.v2"
)

const (
	// APITimeFormat - clickhouse compatibility time format
	APITimeFormat  = "2006-01-02 15:04:05"
	InProgressText = "in progress"
)

type APIServer struct {
	c                       *cli.App
	configPath              string
	config                  *config.Config
	server                  *http.Server
	restart                 chan struct{}
	status                  *AsyncStatus
	metrics                 Metrics
	routes                  []string
	clickhouseBackupVersion string
}

type AsyncStatus struct {
	commands []ActionRow
	sync.RWMutex
}

type ActionRow struct {
	Command string `json:"command"`
	Status  string `json:"status"`
	Start   string `json:"start,omitempty"`
	Finish  string `json:"finish,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (status *AsyncStatus) start(command string) {
	status.Lock()
	defer status.Unlock()
	status.commands = append(status.commands, ActionRow{
		Command: command,
		Start:   time.Now().Format(APITimeFormat),
		Status:  InProgressText,
	})
}

func (status *AsyncStatus) inProgress() bool {
	status.RLock()
	defer status.RUnlock()
	n := len(status.commands) - 1
	if n < 0 {
		return false
	}
	return status.commands[n].Status == InProgressText
}

func (status *AsyncStatus) stop(err error) {
	status.Lock()
	defer status.Unlock()
	n := len(status.commands) - 1
	s := "success"
	if err != nil {
		s = "error"
		status.commands[n].Error = err.Error()
	}
	status.commands[n].Status = s
	status.commands[n].Finish = time.Now().Format(APITimeFormat)
}

func (status *AsyncStatus) status() []ActionRow {
	status.RLock()
	defer status.RUnlock()
	return status.commands
}

var (
	ErrAPILocked = errors.New("another operation is currently running")
)

// Server - expose CLI commands as REST API
func Server(c *cli.App, cfg *config.Config, configPath string, clickhouseBackupVersion string) error {
	api := APIServer{
		c:                       c,
		configPath:              configPath,
		config:                  cfg,
		restart:                 make(chan struct{}),
		status:                  &AsyncStatus{},
		clickhouseBackupVersion: clickhouseBackupVersion,
	}
	api.metrics = setupMetrics()
	if err := api.updateSizeOfLastBackup(); err != nil {
		return err
	}
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, os.Interrupt, syscall.SIGTERM)
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, os.Interrupt, syscall.SIGHUP)
	log.Infof("Starting API server on %s", api.config.API.ListenAddr)
	if err := api.Restart(); err != nil {
		return err
	}

	for {
		select {
		case <-api.restart:
			if err := api.Restart(); err != nil {
				log.Errorf("Failed to restarting API server: %v", err)
				continue
			}
			log.Infof("Reloaded by HTTP")
		case <-sighup:
			newCfg, err := config.LoadConfig(configPath)
			if err != nil {
				log.Errorf("Failed to read config: %v", err)
				continue
			}
			api.config = newCfg
			if err := api.Restart(); err != nil {
				log.Errorf("Failed to restarting API server: %v", err)
				continue
			}
			log.Info("Reloaded by SYSHUP")
		case <-sigterm:
			log.Info("Stopping API server")
			return api.server.Close()
		}
	}
}

func (api *APIServer) Restart() error {
	server := api.setupAPIServer(api.config)
	if api.server != nil {
		api.server.Close()
	}
	api.server = server
	if api.config.API.Secure {
		go api.server.ListenAndServeTLS(api.config.API.CertificateFile, api.config.API.PrivateKeyFile)
		return nil
	}
	go api.server.ListenAndServe()
	return nil
}

// setupAPIServer - resister API routes
func (api *APIServer) setupAPIServer(cfg *config.Config) *http.Server {
	r := mux.NewRouter()
	r.Use(api.basicAuthMidleware)
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "", fmt.Errorf("404 Not Found"))
	})
	r.MethodNotAllowedHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "", fmt.Errorf("405 Method Not Allowed"))
	})

	r.HandleFunc("/", api.httpRootHandler).Methods("GET")

	r.HandleFunc("/backup/tables", api.httpTablesHandler).Methods("GET")
	r.HandleFunc("/backup/list", api.httpListHandler).Methods("GET")
	r.HandleFunc("/backup/create", api.httpCreateHandler).Methods("POST")
	r.HandleFunc("/backup/upload/{name}", api.httpUploadHandler).Methods("POST")
	r.HandleFunc("/backup/download/{name}", api.httpDownloadHandler).Methods("POST")
	r.HandleFunc("/backup/restore/{name}", api.httpRestoreHandler).Methods("POST")
	r.HandleFunc("/backup/delete/{where}/{name}", api.httpDeleteHandler).Methods("POST")
	r.HandleFunc("/backup/config/default", httpConfigDefaultHandler).Methods("GET")
	r.HandleFunc("/backup/config", api.httpConfigHandler).Methods("GET")
	r.HandleFunc("/backup/config", api.httpConfigUpdateHandler).Methods("POST")
	r.HandleFunc("/backup/status", api.httpBackupStatusHandler).Methods("GET")

	r.HandleFunc("/backup/actions", api.actionsLog).Methods("GET")
	r.HandleFunc("/backup/actions", api.actions).Methods("POST")

	var routes []string
	r.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		t, err := route.GetPathTemplate()
		if err != nil {
			return err
		}
		routes = append(routes, t)
		return nil
	})
	api.routes = routes
	registerMetricsHandlers(r, cfg.API.EnableMetrics, cfg.API.EnablePprof)
	srv := &http.Server{
		Addr:    cfg.API.ListenAddr,
		Handler: r,
	}
	return srv
}

func (api *APIServer) basicAuthMidleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		query := r.URL.Query()
		if u, exist := query["user"]; exist {
			user = u[0]
		}
		if p, exist := query["pass"]; exist {
			pass = p[0]
		}
		if (user != api.config.API.Username) || (pass != api.config.API.Password) {
			w.Header().Set("WWW-Authenticate", "Basic realm=\"Provide username and password\"")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("401 Unauthorized\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CREATE TABLE system.backup_actions (command String, start DateTime, finish DateTime, status String, error String) ENGINE=URL('http://127.0.0.1:7171/backup/actions?user=user&pass=pass', JSONEachRow)
// INSERT INTO system.backup_actions (command) VALUES ('create backup_name')
// INSERT INTO system.backup_actions (command) VALUES ('upload backup_name')
func (api *APIServer) actions(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", err)
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "", fmt.Errorf("empty request"))
		return
	}
	lines := bytes.Split(body, []byte("\n"))
	for _, line := range lines {
		row := ActionRow{}
		if err := json.Unmarshal(line, &row); err != nil {
			writeError(w, http.StatusBadRequest, "", err)
			return
		}
		log.Infof(row.Command)
		args, err := shlex.Split(row.Command)
		if err != nil {
			writeError(w, http.StatusBadRequest, "", err)
			return
		}
		command := args[0]
		switch command {
		case "create", "restore", "upload", "download":
			if api.status.inProgress() {
				log.Info(ErrAPILocked.Error())
				writeError(w, http.StatusLocked, row.Command, ErrAPILocked)
				return
			}
			start := time.Now()
			api.metrics.LastStart[command].Set(float64(start.Unix()))
			defer api.metrics.LastDuration[command].Set(float64(time.Since(start).Nanoseconds()))
			defer api.metrics.LastFinish[command].Set(float64(time.Now().Unix()))

			go func() {
				api.status.start(row.Command)
				err := api.c.Run(append([]string{"clickhouse-backup", "-c", api.configPath}, args...))
				defer api.status.stop(err)
				if err != nil {
					api.metrics.FailedCounter[command].Inc()
					log.Error(err.Error())
					return
				}
				if err := api.updateSizeOfLastBackup(); err != nil {
					log.Errorf("update size: %v", err)
				}
				api.metrics.SuccessfulCounter[command].Inc()
			}()
			sendJSONEachRow(w, http.StatusCreated, struct {
				Status    string `json:"status"`
				Operation string `json:"operation"`
			}{
				Status:    "acknowledged",
				Operation: row.Command,
			})
			return
		case "delete":
			if api.status.inProgress() {
				log.Info(ErrAPILocked.Error())
				writeError(w, http.StatusLocked, row.Command, ErrAPILocked)
				return
			}
			api.status.start(row.Command)
			err := api.c.Run(append([]string{"clickhouse-backup", "-c", api.configPath}, args...))
			defer api.status.stop(err)
			if err != nil {
				writeError(w, http.StatusBadRequest, row.Command, err)
				log.Error(err.Error())
				return
			}
			log.Info("OK")
			if err := api.updateSizeOfLastBackup(); err != nil {
				log.Errorf("update size: %v", err)
			}
			sendJSONEachRow(w, http.StatusCreated, struct {
				Status    string `json:"status"`
				Operation string `json:"operation"`
			}{
				Status:    "ok",
				Operation: row.Command,
			})
			return
		default:
			writeError(w, http.StatusBadRequest, row.Command, fmt.Errorf("unknown command"))
			return
		}
	}
}

func (api *APIServer) actionsLog(w http.ResponseWriter, r *http.Request) {
	sendJSONEachRow(w, http.StatusOK, api.status.status())
}

// httpRootHandler - display API index
func (api *APIServer) httpRootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	fmt.Fprintln(w, "Documentation: https://github.com/AlexAkulov/clickhouse-backup#api-configuration")
	for _, r := range api.routes {
		fmt.Fprintln(w, r)
	}
}

// httpConfigDefaultHandler - display the default config. Same as CLI: clickhouse-backup default-config
func httpConfigDefaultHandler(w http.ResponseWriter, r *http.Request) {
	defaultConfig := config.DefaultConfig()
	body, err := yaml.Marshal(defaultConfig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "default-config", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	fmt.Fprintln(w, string(body))
}

// httpConfigDefaultHandler - display the currently running config
func (api *APIServer) httpConfigHandler(w http.ResponseWriter, r *http.Request) {
	config := api.config
	config.ClickHouse.Password = "***"
	config.API.Password = "***"
	config.S3.SecretKey = "***"
	config.GCS.CredentialsJSON = "***"
	config.COS.SecretKey = "***"
	config.FTP.Password = "***"
	body, err := yaml.Marshal(&config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	fmt.Fprintln(w, string(body))
}

// httpConfigDefaultHandler - update the currently running config
func (api *APIServer) httpConfigUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusServiceUnavailable, "update", ErrAPILocked)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("reading body error: %v", err))
		return
	}

	newConfig := config.DefaultConfig()
	if err := yaml.Unmarshal(body, &newConfig); err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("error parsing new config: %v", err))
		return
	}

	if err := config.ValidateConfig(newConfig); err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("error validating new config: %v", err))
		return
	}
	log.Info("Applying new valid config")
	api.config = newConfig
	api.restart <- struct{}{}
}

// httpTablesHandler - displaylist of tables
func (api *APIServer) httpTablesHandler(w http.ResponseWriter, r *http.Request) {
	tables, err := backup.GetTables(*api.config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "tables", err)
		return
	}
	sendJSONEachRow(w, http.StatusOK, tables)
}

// httpTablesHandler - display list of all backups stored locally and remotely
// CREATE TABLE system.backup_list (name String, created DateTime, size Int64, location String) ENGINE=URL('http://127.0.0.1:7171/backup/list?user=user&pass=pass', JSONEachRow)
// ??? INSERT INTO system.backup_list (name,location) VALUES ('backup_name', 'remote') - upload backup
// ??? INSERT INTO system.backup_list (name) VALUES ('backup_name') - create backup
func (api *APIServer) httpListHandler(w http.ResponseWriter, r *http.Request) {
	type backupJSON struct {
		Name     string `json:"name"`
		Created  string `json:"created"`
		Size     int64  `json:"size,omitempty"`
		Location string `json:"location"`
	}
	backupsJSON := make([]backupJSON, 0)
	localBackups, err := backup.GetLocalBackups(api.config)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "list", err)
		return
	}
	for _, b := range localBackups {
		backupsJSON = append(backupsJSON, backupJSON{
			Name:     b.BackupName,
			Created:  b.CreationDate.Format(APITimeFormat),
			Location: "local",
		})
	}
	if api.config.General.RemoteStorage != "none" {
		remoteBackups, err := backup.GetRemoteBackups(api.config)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list", err)
			return
		}
		for _, b := range remoteBackups {
			backupsJSON = append(backupsJSON, backupJSON{
				Name:     b.BackupName,
				Created:  b.CreationDate.Format(APITimeFormat),
				Size:     b.Size,
				Location: "remote",
			})
		}
	}
	sendJSONEachRow(w, http.StatusOK, backupsJSON)
}

// httpCreateHandler - create a backup
func (api *APIServer) httpCreateHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusLocked, "create", ErrAPILocked)
		return
	}
	tablePattern := ""
	backupName := backup.NewBackupName()
	schemaOnly := false

	query := r.URL.Query()
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if name, exist := query["name"]; exist {
		backupName = name[0]
	}
	if schema, exist := query["schema"]; exist {
		schemaOnly, _ = strconv.ParseBool(schema[0])
	}

	go func() {
		api.status.start("create")
		start := time.Now()
		api.metrics.LastStart["create"].Set(float64(start.Unix()))
		defer api.metrics.LastDuration["create"].Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastFinish["create"].Set(float64(time.Now().Unix()))
		err := backup.CreateBackup(api.config, backupName, tablePattern, schemaOnly, api.clickhouseBackupVersion)
		defer api.status.stop(err)
		if err != nil {
			api.metrics.FailedCounter["create"].Inc()
			log.Errorf("CreateBackup error: %v", err)
			return
		}
		if err := api.updateSizeOfLastBackup(); err != nil {
			log.Errorf("update size: %v", err)
		}
		api.metrics.SuccessfulCounter["create"].Inc()
	}()
	sendJSONEachRow(w, http.StatusCreated, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "acknowledged",
		Operation:  "create",
		BackupName: backupName,
	})
}

// httpUploadHandler - upload a backup to remote storage
func (api *APIServer) httpUploadHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusLocked, "upload", ErrAPILocked)
		return
	}
	vars := mux.Vars(r)
	diffFrom := ""
	query := r.URL.Query()
	if df, exist := query["diff-from"]; exist {
		diffFrom = df[0]
	}
	name := vars["name"]
	tablePattern := ""
	schemaOnly := false
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if schema, exist := query["schema"]; exist {
		schemaOnly, _ = strconv.ParseBool(schema[0])
	}
	go func() {
		api.status.start("upload")
		start := time.Now()
		api.metrics.LastStart["upload"].Set(float64(start.Unix()))
		defer api.metrics.LastDuration["upload"].Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastFinish["upload"].Set(float64(time.Now().Unix()))
		err := backup.Upload(api.config, name, tablePattern, diffFrom, schemaOnly)
		api.status.stop(err)
		if err != nil {
			log.Errorf("Upload error: %+v\n", err)
			api.metrics.FailedCounter["upload"].Inc()
			return
		}
		if err := api.updateSizeOfLastBackup(); err != nil {
			log.Errorf("update size: %v", err)
		}
		api.metrics.SuccessfulCounter["upload"].Inc()
	}()
	sendJSONEachRow(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
		BackupFrom string `json:"backup_from,omitempty"`
		Diff       bool   `json:"diff"`
	}{
		Status:     "acknowledged",
		Operation:  "upload",
		BackupName: name,
		BackupFrom: diffFrom,
		Diff:       diffFrom != "",
	})
}

// httpRestoreHandler - restore a backup from local storage
func (api *APIServer) httpRestoreHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusLocked, "restore", ErrAPILocked)
		return
	}

	vars := mux.Vars(r)
	tablePattern := ""
	schemaOnly := false
	dataOnly := false
	dropTable := false

	query := r.URL.Query()
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if _, exist := query["schema"]; exist {
		schemaOnly = true
	}
	if _, exist := query["data"]; exist {
		dataOnly = true
	}
	if _, exist := query["drop"]; exist {
		dropTable = true
	}
	if _, exist := query["rm"]; exist {
		dropTable = true
	}
	go func() {
		api.status.start("restore")
		start := time.Now()
		api.metrics.LastStart["restore"].Set(float64(start.Unix()))
		defer api.metrics.LastDuration["restore"].Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastFinish["restore"].Set(float64(time.Now().Unix()))
		err := backup.Restore(api.config, vars["name"], tablePattern, schemaOnly, dataOnly, dropTable)
		api.status.stop(err)
		if err != nil {
			log.Errorf("Download error: %+v\n", err)
			api.metrics.FailedCounter["restore"].Inc()
			return
		}
		api.metrics.SuccessfulCounter["restore"].Inc()
	}()
	sendJSONEachRow(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "acknowledged",
		Operation:  "restore",
		BackupName: vars["name"],
	})
}

// httpDownloadHandler - download a backup from remote to local storage
func (api *APIServer) httpDownloadHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusLocked, "download", ErrAPILocked)
		return
	}
	vars := mux.Vars(r)
	name := vars["name"]
	query := r.URL.Query()
	tablePattern := ""
	schemaOnly := false
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if _, exist := query["schema"]; exist {
		schemaOnly = true
	}
	go func() {
		api.status.start("download")
		start := time.Now()
		api.metrics.LastStart["download"].Set(float64(start.Unix()))
		defer api.metrics.LastDuration["download"].Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastFinish["download"].Set(float64(time.Now().Unix()))
		err := backup.Download(api.config, name, tablePattern, schemaOnly)
		api.status.stop(err)
		if err != nil {
			log.Errorf("Download error: %+v\n", err)
			api.metrics.FailedCounter["download"].Inc()
			return
		}
		if err := api.updateSizeOfLastBackup(); err != nil {
			log.Errorf("update size: %v", err)
		}
		api.metrics.SuccessfulCounter["download"].Inc()
	}()
	sendJSONEachRow(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "acknowledged",
		Operation:  "download",
		BackupName: name,
	})
}

// httpDeleteHandler - delete a backup from local or remote storage
func (api *APIServer) httpDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if api.status.inProgress() {
		log.Info(ErrAPILocked.Error())
		writeError(w, http.StatusLocked, "delete", ErrAPILocked)
		return
	}
	api.status.start("delete")
	var err error
	vars := mux.Vars(r)
	switch vars["where"] {
	case "local":
		err = backup.RemoveBackupLocal(api.config, vars["name"])
	case "remote":
		err = backup.RemoveBackupRemote(api.config, vars["name"])
	default:
		err = fmt.Errorf("Backup location must be 'local' or 'remote'")
	}
	api.status.stop(err)
	if err != nil {
		log.Errorf("delete backup error: %+v\n", err)
		writeError(w, http.StatusInternalServerError, "delete", err)
		return
	}
	if err := api.updateSizeOfLastBackup(); err != nil {
		log.Errorf("update size: %v", err)
	}
	sendJSONEachRow(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
		Location   string `json:"location"`
	}{
		Status:     "success",
		Operation:  "delete",
		BackupName: vars["name"],
		Location:   vars["where"],
	})
}

func (api *APIServer) httpBackupStatusHandler(w http.ResponseWriter, r *http.Request) {
	sendJSONEachRow(w, http.StatusOK, api.status.status())
}

func (api *APIServer) updateSizeOfLastBackup() error {
	if !api.config.API.EnableMetrics {
		return nil
	}
	localBackups, err := backup.GetLocalBackups(api.config)
	if err != nil {
		return err
	}
	if len(localBackups) > 0 {
		api.metrics.LastBackupSizeLocal.Set(float64(localBackups[len(localBackups)-1].Size))
	} else {
		api.metrics.LastBackupSizeLocal.Set(0)
	}
	if api.config.General.RemoteStorage == "none" {
		return nil
	}
	remoteBackups, err := backup.GetRemoteBackups(api.config)
	if err != nil {
		return err
	}
	if len(remoteBackups) > 0 {
		api.metrics.LastBackupSizeRemote.Set(float64(remoteBackups[len(remoteBackups)-1].Size))
	} else {
		api.metrics.LastBackupSizeRemote.Set(0)
	}
	return nil
}

func registerMetricsHandlers(r *mux.Router, enablemetrics bool, enablepprof bool) {
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sendJSONEachRow(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{
			Status: "OK",
		})
	})
	if enablemetrics {
		r.Handle("/metrics", promhttp.Handler())
	}
	if enablepprof {
		r.HandleFunc("/debug/pprof/", pprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.Handle("/debug/pprof/block", pprof.Handler("block"))
		r.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		r.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		r.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	}
}

type Metrics struct {
	SuccessfulCounter map[string]prometheus.Counter
	FailedCounter     map[string]prometheus.Counter
	LastStart         map[string]prometheus.Gauge
	LastFinish        map[string]prometheus.Gauge
	LastDuration      map[string]prometheus.Gauge

	LastBackupSizeLocal  prometheus.Gauge
	LastBackupSizeRemote prometheus.Gauge
}

// setupMetrics - resister prometheus metrics
func setupMetrics() Metrics {
	m := Metrics{}
	successfulCounter := map[string]prometheus.Counter{}
	failedCounter := map[string]prometheus.Counter{}
	lastStart := map[string]prometheus.Gauge{}
	lastFinish := map[string]prometheus.Gauge{}
	lastDuration := map[string]prometheus.Gauge{}

	for _, command := range []string{"create", "upload", "download", "restore"} {
		successfulCounter[command] = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "clickhouse_backup",
			Name:      fmt.Sprintf("successful_%ss", command),
			Help:      fmt.Sprintf("Counter of successful %ss backup", command),
		})
		failedCounter[command] = prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "clickhouse_backup",
			Name:      fmt.Sprintf("failed_%ss", command),
			Help:      fmt.Sprintf("Counter of failed %ss backup", command),
		})
		lastStart[command] = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "clickhouse_backup",
			Name:      fmt.Sprintf("last_%s_start", command),
			Help:      fmt.Sprintf("Last backup %s start timestamp", command),
		})
		lastFinish[command] = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "clickhouse_backup",
			Name:      fmt.Sprintf("last_%s_finish", command),
			Help:      fmt.Sprintf("Last backup %s finish timestamp", command),
		})
		lastDuration[command] = prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "clickhouse_backup",
			Name:      fmt.Sprintf("last_%s_duration", command),
			Help:      fmt.Sprintf("Backup %s duration in nanoseconds", command),
		})
	}

	m.SuccessfulCounter = successfulCounter
	m.FailedCounter = failedCounter
	m.LastStart = lastStart
	m.LastFinish = lastFinish
	m.LastDuration = lastDuration

	m.LastBackupSizeLocal = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_size_local",
		Help:      "Last local backup size in bytes",
	})
	m.LastBackupSizeRemote = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_size_remote",
		Help:      "Last remote backup size in bytes",
	})

	prometheus.MustRegister(
		m.SuccessfulCounter["create"],
		m.FailedCounter["create"],
		m.LastStart["create"],
		m.LastFinish["create"],
		m.LastDuration["create"],

		m.SuccessfulCounter["upload"],
		m.FailedCounter["upload"],
		m.LastStart["upload"],
		m.LastFinish["upload"],
		m.LastDuration["upload"],

		m.SuccessfulCounter["download"],
		m.FailedCounter["download"],
		m.LastStart["download"],
		m.LastFinish["download"],
		m.LastDuration["download"],

		m.SuccessfulCounter["restore"],
		m.FailedCounter["restore"],
		m.LastStart["restore"],
		m.LastFinish["restore"],
		m.LastDuration["restore"],

		m.LastBackupSizeLocal,
		m.LastBackupSizeRemote,
	)
	return m
}
