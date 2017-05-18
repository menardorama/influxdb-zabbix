package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	toml "github.com/BurntSushi/toml"

	cfg "github.com/menardorama/influxdb-zabbix/config"
	pgsql "github.com/menardorama/influxdb-zabbix/input/postgresql"
	log "github.com/menardorama/influxdb-zabbix/log"
	influx "github.com/menardorama/influxdb-zabbix/output/influxdb"
)

var exitChan = make(chan int)

var wg sync.WaitGroup

var fConfig = flag.String("config",
	"influxdb-zabbix.conf",
	"the configuration file in TOML format")

type TOMLConfig cfg.TOMLConfig

var config TOMLConfig

type DynMap map[string]interface{}

type Param struct {
	input  Input
	output Output
}

type Input struct {
	provider  string
	address   string
	tablename string
	interval  int
	mapTables *MapTable
}

type Output struct {
	address   string
	database  string
	username  string
	password  string
	precision string
}

// type Postgres struct {
//  	input Input
// }

type InfluxDB struct {
	output Output
}

type Registry struct {
	Table     string
	Startdate string
}

type MapTable map[string]string

var mapTables = make(MapTable)

var mu sync.Mutex

//
// Utils
//
func rightPad(s string, padStr string, pLen int) string {
	return s + strings.Repeat(padStr, pLen)
}

//
// Listen to System Signals
//
func listenToSystemSignals() {
	signalChan := make(chan os.Signal, 1)
	code := 0

	signal.Notify(signalChan, os.Interrupt)
	signal.Notify(signalChan, os.Kill)
	signal.Notify(signalChan, syscall.SIGTERM)

	select {
	case sig := <-signalChan:
		log.Info("Received signal %s. shutting down", sig)
	case code = <-exitChan:
		switch code {
		case 0:
			log.Info("Shutting down")
		default:
			log.Warn("Shutting down")
		}
	}
	log.Close()
	os.Exit(code)
}

//
// Init logging
//
func (config *TOMLConfig) initLogging() {
	var LogModes []string
	var LogConfigs []string

	// Log Modes
	LogModes = strings.Split(config.Logging.Modes, ",")
	LogConfigs = make([]string, len(LogModes))

	for i, mode := range LogModes {
		mode = strings.TrimSpace(mode)
		//fmt.Printf("Logging mode: %s\n", mode)

		// Log Level
		var levelName string
		if mode == "console" {
			levelName = config.Logging.LevelConsole
		} else {
			levelName = config.Logging.LevelFile
		}

		level, ok := log.LogLevels[levelName]
		if !ok {
			log.Fatal(4, "Unknown log level: %s", levelName)
		}
		// Generate log configuration
		switch mode {
		case "console":
			LogConfigs[i] = fmt.Sprintf(`{"level":%v,"formatting":%v}`,
				level,
				config.Logging.Formatting)
		case "file":
			LogConfigs[i] = fmt.Sprintf(`{"level":%v,"filename":"%s","rotate":%v,"maxlines":%d,"maxsize":%d,"daily":%v,"maxdays":%d}`,
				level,
				config.Logging.FileName,
				config.Logging.LogRotate,
				config.Logging.MaxLines,
				1<<uint(config.Logging.MaxSizeShift),
				config.Logging.DailyRotate,
				config.Logging.MaxDays)
		}
		log.NewLogger(int64(config.Logging.BufferLen), mode, LogConfigs[i])
		log.Trace("Log Mode: %s(%s)", strings.Title(mode), levelName)
	}
}

// Validate adds default value, validates the config data
// and returns an error describing any problems or nil.
func (toml *TOMLConfig) Validate() error {

	if toml.Registry.FileName == "" {
		toml.Registry.FileName = cfg.DefaultRegistryFileName
	}
	if toml.Logging.FileName == "" {
		toml.Logging.FileName = cfg.DefaultLogFileName
	}
	if toml.Logging.Modes == "" {
		toml.Logging.Modes = cfg.DefaultModes
	}
	if toml.Logging.BufferLen == 0 {
		toml.Logging.BufferLen = cfg.DefaultBufferLen
	}
	if toml.Logging.LevelConsole == "" {
		toml.Logging.LevelConsole = cfg.DefaultLevelConsole
	}
	if toml.Logging.LevelFile == "" {
		toml.Logging.LevelFile = cfg.DefaultLevelFile
	}
	if toml.Logging.MaxLines == 0 {
		toml.Logging.MaxLines = cfg.DefaultMaxLines
	}
	if toml.Logging.MaxSizeShift == 0 {
		toml.Logging.MaxSizeShift = cfg.DefaultMaxSizeShift
	}
	if toml.Logging.MaxDays == 0 {
		toml.Logging.MaxDays = cfg.DefaultMaxDays
	}
	if toml.Polling.Interval == 0 {
		toml.Polling.Interval = cfg.DefaultPollingInterval
	}
	if toml.Polling.IntervalIfError == 0 {
		toml.Polling.IntervalIfError = cfg.DefaultPollingIntervalIfError
	}
	if toml.InfluxDB.Url == "" {
		toml.InfluxDB.Url = cfg.DefaultInfluxDBUrl
	}
	if toml.InfluxDB.Database == "" {
		toml.InfluxDB.Database = cfg.DefaultInfluxDBDatabase
	}
	if toml.InfluxDB.Precision == "" {
		toml.InfluxDB.Precision = cfg.DefaultInfluxDBPrecision
	}
	if toml.InfluxDB.TimeOut == 0 {
		toml.InfluxDB.TimeOut = cfg.DefaultInfluxDBTimeOut
	}

	fmterr := fmt.Errorf

	// InfluxDB
	fullUrl := strings.Replace(toml.InfluxDB.Url, "http://", "", -1)

	host, portStr, err := net.SplitHostPort(fullUrl)
	if err != nil {
		return fmterr("Validation failed : InfluxDB url must be formatted as host:port but "+
			"was '%s' (%v).", toml.InfluxDB.Url, err)
	}
	if len(host) == 0 {
		return fmterr("Validation failed : InfluxDB url value ('%s') is missing a host.",
			toml.InfluxDB.Url)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmterr("Validation failed : InfluxDB url port value ('%s') must be a number "+
			"(%v).", portStr, err)
	}
	if port < 1 || port > 65535 {
		return fmterr("Validation failed : InfluxDB url port must be within [1-65535] but "+
			"was '%d'.", port)
	}

	// Zabbix
	zabbixes := toml.Zabbix
	if len(zabbixes) == 0 {
		return fmterr("Validation failed : You must at least define one Zabbix database provider.")
	}
	if len(zabbixes) > 1 {
		return fmterr("Validation failed : You can only define one Zabbix database.")
	}

	for dbprov, zabbix := range zabbixes {
		if zabbix.Address == "" {
			return fmterr("Validation failed : You must at least define a Zabbix database address for provider %s.", dbprov)
		}
	}

	// Zabbix tables
	tables := toml.Tables
	if len(tables) == 0 {
		return fmterr("Validation failed : You must at least define one table.")
	}
	var activeTablesCount = 0
	for tableName, table := range tables {
		if table.Active {
			activeTablesCount += 1
		}
		if table.Interval < 15 {
			toml.Tables[tableName].Interval = 15 // override
		}

		if len(table.Startdate) > 0 {
			// validate date format
			layout := "2006-01-02T15:04:05"
			_, err := time.Parse(
				layout,
				table.Startdate)
			if err != nil {
				return fmterr("Validation failed : Startdate for table %s is not well formatted.", tableName)
			}
		}
	}
	if activeTablesCount == 0 {
		return fmterr("Validation failed : You must at least define one active table.")
	}

	return nil
}

//
// Gather data
//
func (p *Param) gatherData() error {
	// read registry
	if err := readRegistry(); err != nil {
		fmt.Println(err)
		return err
	}
	start := time.Now()
	startdate := mapTables.Get(p.input.tablename)

	if len(startdate) == 0 {
		log.Fatal(1, "No startdate defined for table %s", p.input.tablename)
		return nil
	}

	if p.input.provider != "postgresql" {
		log.Fatal(1, "Provider %s is not yet supported", p.input.provider)
		return nil
	}

	// <--  Extract
	ext := pgsql.NewExtracter(
		p.input.address,
		p.input.tablename,
		startdate)

	if err := ext.Extract(); err != nil {
		log.Error(1, "Error while executing script: %s", err)
		return err
	}

	var rows int = len(ext.Result)
	var tlen int = len(p.input.tablename)

	log.Info(
		fmt.Sprintf(
			"<-- Extract | %s| %v rows | took %s",
			rightPad(p.input.tablename, " ", 20-tlen),
			rows,
			time.Since(start)))

	/// no row, no load
	if rows == 0 {
		log.Info(
			fmt.Sprintf(
				"--> Load    | %s| No data",
				rightPad(p.input.tablename, " ", 20-tlen)))

		// Save registry
		saveRegistry(p.input.tablename, ext.Enddate)

		log.Info(
			fmt.Sprintf(
				"--- Waiting | %s| %v sec ",
				rightPad(p.input.tablename, " ", 20-len(p.input.tablename)),
				p.input.interval))
		return nil
	}

	// --> Load
	start = time.Now()
	batchNumber := 100000
	inlineData := ""

	if rows <= batchNumber {
		inlineData = strings.Join(ext.Result[:], "\n")

		loa := influx.NewLoader(
			fmt.Sprintf("%s/write?db=%s&precision=%s",
				p.output.address,
				p.output.database,
				p.output.precision),
                                p.output.username,
                                p.output.password,
                                inlineData)
		err := loa.Load()
		if err != nil {
			log.Error(1, "Error while loading data: %s", err)
			return err
		}

		log.Info(
			fmt.Sprintf(
				"--> Load    | %s| %v rows | took %s",
				rightPad(p.input.tablename, " ", 20-tlen),
				rows,
				time.Since(start)))

	} else { // multiple batches

		var batches float64 = float64(rows) / float64(batchNumber)
		var batchesCeiled float64 = math.Ceil(batches)
		var batchLoops int = 1
		var minRange int = 0
		var maxRange int = 0

		for batches > 0 { // while
			if batchLoops == 1 {
				minRange = 0
			} else {
				minRange = maxRange + 1
			}

			maxRange = batchLoops * batchNumber
			if maxRange > rows {
				maxRange = rows - 1
			}

			// create slide
			datapart := []string{}
			for i := minRange; i <= maxRange; i++ {
				datapart = append(datapart, ext.Result[i])
			}

			inlineData = strings.Join(datapart[:], "\n")

			start = time.Now()
			loa := influx.NewLoader(
				fmt.Sprintf(
					"%s/write?db=%s&precision=%s",
					p.output.address,
					p.output.database,
					p.output.precision),
                                p.output.username,
                                p.output.password,
                                inlineData)

			err := loa.Load()
			if err != nil {
				log.Error(1, "Error while loading data: %s", err)
				return err
			}

			prettyTableName := fmt.Sprintf("%s (%v/%v)",
				p.input.tablename,
				batchLoops,
				batchesCeiled)
			tlen = len(prettyTableName)

			log.Info(fmt.Sprintf("--> Load    | %s| %v rows | took %s",
				prettyTableName,
				len(datapart),
				time.Since(start)))

			batchLoops += 1
			batches -= 1
		}
	}

	// Save registry
	saveRegistry(p.input.tablename, ext.Enddate)
	tlen = len(p.input.tablename)
	log.Info(fmt.Sprintf("--- Waiting | %s| %v sec ",
		rightPad(p.input.tablename, " ", 20-tlen),
		p.input.interval))

	return nil
}

//
// Gather data loop
//
func (p *Param) gather() error {

	for {
		err := p.gatherData()
		if err != nil {
			return err
		}

		time.Sleep(time.Duration(p.input.interval) * time.Second)
	}
	return nil
}

func parseConfig() error {
	if _, err := toml.DecodeFile(*fConfig, &config); err != nil {
		return err
	}
	return nil
}

func validateConfig() error {
	if err := (&config).Validate(); err != nil {
		return err
	}
	return nil
}

func createRegistry() error {

	if len(config.Tables) == 0 {
		err := errors.New("No tables in configuration")
		check(err)
	}

	regEntries := make([]Registry, len(config.Tables))

	var idx int = 0
	for _, table := range config.Tables {
		var reg Registry
		reg.Table = table.Name
		reg.Startdate = table.Startdate
		regEntries[idx] = reg
		idx += 1
	}

	// write JSON file
	registryOutJson, _ := json.MarshalIndent(regEntries, "", "    ")
	ioutil.WriteFile(config.Registry.FileName, registryOutJson, 0777)

	log.Trace(fmt.Sprintf("------ Registry file is now created"))

	return nil
}

func readRegistry() error {

	if _, err := ioutil.ReadFile(config.Registry.FileName); err != nil {
		createRegistry() // create if not exist
	}

	registryJson, err := ioutil.ReadFile(config.Registry.FileName)
	check(err)

	// parse JSON
	regEntries := make([]Registry, 0)
	if err := json.Unmarshal(registryJson, &regEntries); err != nil {
		return err
	}

	for i := 0; i < len(regEntries); i++ {
		tableName := regEntries[i].Table
		startdate := regEntries[i].Startdate
		mapTables.Set(tableName, startdate)
		// mu.Lock()
		// mapTables[tableName] = startdate
		// mu.Unlock()
	}

	return nil
}

func saveRegistry(tableName string, lastClock string) error {

	// read  file
	registryJson, err := ioutil.ReadFile(config.Registry.FileName)
	check(err)

	// parse JSON
	regEntries := make([]Registry, 0)
	if err := json.Unmarshal(registryJson, &regEntries); err != nil {
		return err
	}
	var found bool = false
	for i := 0; i < len(regEntries); i++ {
		if regEntries[i].Table == tableName {
			regEntries[i].Startdate = lastClock
			found = true
		}
	}
	// if not found, create it
	if found == false {
		regEntries = append(regEntries, Registry{tableName, lastClock})
	}

	// write JSON file
	registryOutJson, _ := json.MarshalIndent(regEntries, "", "    ")
	ioutil.WriteFile(config.Registry.FileName, registryOutJson, 0777)

	return nil
}

func (mt MapTable) Set(key string, value string) {
	mu.Lock()
	defer mu.Unlock()
	mt[key] = value
}
func (mt MapTable) Get(key string) string {
	if len(mt) > 0 {
		mu.Lock()
		defer mu.Unlock()
		return mt[key]
	}
	return ""
}

//
// Init
//
func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

//
// Error check
//
func check(e error) {
	if e != nil {
		panic(e)
	}
}

//
// Main
//
func main() {

	// command-line flag parsing
	flag.Parse()

	// read configuration file
	if err := parseConfig(); err != nil {
		fmt.Println(err)
		return
	}
	// validate configuration file
	if err := validateConfig(); err != nil {
		fmt.Println(err)
		return
	}

	// init global logging
	config.initLogging()

	// listen to System Signals
	go listenToSystemSignals()

	// read registry
	if err := readRegistry(); err != nil {
		log.Error(0, err.Error())
		return
	}

	log.Info("***** Starting influxdb-zabbix *****")

	// define list of enabled tables
	log.Trace("--- Active tables:")
	var tablesEnabled = []*cfg.Table{}
	for _, table := range config.Tables {
		if table.Active {
			log.Trace(
				fmt.Sprintf(
					"------ %s with an interval of %v sec",
					table.Name,
					table.Interval))

			tablesEnabled = append(tablesEnabled, table)
		}
	}
	// input source
	var source = config.Zabbix
	var provider string
	var address string

	for prov, zabbix := range source {
		provider = prov
		address = zabbix.Address
	}
	log.Trace(fmt.Sprintf("--- Provider:"))
	log.Trace(fmt.Sprintf("------ %s", provider))

	log.Info("--- Start polling")
	for _, t := range tablesEnabled { // foreach active tables

		input := Input{
			provider,
			address,
			t.Name,
			t.Interval,
			&mapTables}

		output := Output{
			config.InfluxDB.Url,
			config.InfluxDB.Database,
			config.InfluxDB.Username,
			config.InfluxDB.Password,
			config.InfluxDB.Precision}

		p := &Param{input, output}

		wg.Add(1)
		go p.gather()
	}
	wg.Wait()
}
