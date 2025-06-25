package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lansfy/gonkex/checker"
	"github.com/lansfy/gonkex/mocks"
	"github.com/lansfy/gonkex/models"
	"github.com/lansfy/gonkex/output"
	"github.com/lansfy/gonkex/output/allure"
	"github.com/lansfy/gonkex/output/terminal"
	"github.com/lansfy/gonkex/runner"
	"github.com/lansfy/gonkex/storage"
	aerospikeStorage "github.com/lansfy/gonkex/storage/addons/aerospike"
	redisStorage "github.com/lansfy/gonkex/storage/addons/redis"
	"github.com/lansfy/gonkex/storage/addons/sqldb"
	"github.com/lansfy/gonkex/testloader/yaml_file"
	"github.com/lansfy/gonkex/variables"

	"github.com/aerospike/aerospike-client-go/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/shlex"
	_ "github.com/lib/pq"
	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v2"
)

type config struct {
	Host          string
	TestsLocation string
	EnvFile       string

	FixturesLocation string
	FixturesDefaults string

	DbType string
	DbDsn  string

	Mocks         string
	MocksPrefix   string
	MocksDefaults string

	PreTestCmd  string
	PreTestWait string

	Allure  bool
	Verbose bool
}

func main() {
	err := runCli()
	if err != nil {
		_, _ = fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

func createMocks(cfg *config) (*mocks.Mocks, error) {
	if cfg.Mocks == "" {
		return nil, nil
	}

	serviceNames := strings.Split(cfg.Mocks, ",")
	m := mocks.NewNop(serviceNames...)
	err := m.Start()
	if err != nil {
		return nil, err
	}

	if cfg.MocksDefaults != "" {
		var data []byte
		data, err = os.ReadFile(cfg.MocksDefaults)
		if err != nil {
			return nil, err
		}

		var definition map[string]interface{}

		err = yaml.UnmarshalStrict(data, &definition)
		if err != nil {
			return nil, err
		}

		err = m.LoadDefinitions(mocks.NewYamlLoader(nil), definition)
		if err != nil {
			return nil, err
		}
	}

	err = m.RegisterEnvironmentVariables(cfg.MocksPrefix)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func runPreTestCommand(cfg *config) (*exec.Cmd, error) {
	if cfg.PreTestCmd == "" {
		return nil, nil
	}

	args, err := shlex.Split(cfg.PreTestCmd)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(args[0], args[1:]...)
	if errors.Is(cmd.Err, exec.ErrDot) {
		// allow to run program from current dir
		cmd.Err = nil
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	return cmd, nil
}

func runPreTestWait(cfg *config) error {
	if cfg.PreTestWait == "" {
		return nil
	}

	delay, err := time.ParseDuration(cfg.PreTestWait)
	if err != nil {
		return err
	}

	time.Sleep(delay)
	return nil
}

func runCli() error {
	cfg := getConfig()
	err := validateConfig(cfg)
	if err != nil {
		return err
	}

	fixtureStorage, err := createStorage(cfg)
	if err != nil {
		return err
	}

	proxyURL, err := proxyURLFromEnv()
	if err != nil {
		return err
	}

	m, err := createMocks(cfg)
	if err != nil {
		return err
	}

	if m != nil {
		defer m.Shutdown()
	}

	if cfg.EnvFile != "" {
		err = runner.RegisterEnvironmentVariables(cfg.EnvFile, false)
		if err != nil {
			return err
		}
	}

	testsRunner := runner.New(
		yaml_file.NewLoader(cfg.TestsLocation),
		&runner.RunnerOpts{
			Host:         cfg.Host,
			Variables:    variables.New(),
			HTTPProxyURL: proxyURL,
			DB:           fixtureStorage,
			Mocks:        m,
			MocksLoader:  mocks.NewYamlLoader(nil),
		},
	)

	testsRunner.AddOutput(terminal.NewOutput(&terminal.OutputOpts{
		ShowSuccess: cfg.Verbose,
	}))

	counter := &testCounter{
		showOutput: !cfg.Verbose,
		testsLoc:   cfg.TestsLocation,
	}
	testsRunner.AddOutput(counter)

	if cfg.Allure {
		var allureOutput *allure.Output
		allureOutput, err = allure.NewOutput("Gonkex", "./allure-results")
		if err != nil {
			return err
		}
		testsRunner.AddOutput(allureOutput)
		defer func() {
			_ = allureOutput.Finalize()
		}()
	}

	if cfg.FixturesDefaults != "" {
		testsRunner.AddCheckers(&fixtureApplier{
			storage:  fixtureStorage,
			location: cfg.FixturesLocation,
			defaults: cfg.FixturesDefaults,
		})
	}

	cmd, err := runPreTestCommand(cfg)
	if err != nil {
		return err
	}

	err = runPreTestWait(cfg)
	if err != nil {
		return err
	}

	if cmd != nil {
		defer func() {
			_ = cmd.Process.Kill()
		}()
	}

	err = testsRunner.Run()
	if err != nil {
		return err
	}

	counter.ShowResult()
	if counter.failed != 0 {
		return fmt.Errorf("one of test failed")
	}

	return nil
}

func createStorage(cfg *config) (storage.StorageInterface, error) {
	if cfg.FixturesLocation == "" {
		return nil, nil
	}

	dbType := strings.ToLower(cfg.DbType)

	switch strings.ToLower(cfg.DbType) {
	case "postgres":
		return createSqlStorage(dbType, sqldb.PostgreSQL, cfg)
	case "mysql":
		return createSqlStorage(dbType, sqldb.MySQL, cfg)
	case "sqlite":
		return createSqlStorage(dbType, sqldb.Sqlite, cfg)
	case "aerospike":
		return createAerospikeStorage(cfg)
	case "redis":
		return createRedisStorage(cfg)
	default:
		return nil, errors.New("you should specify db-dsn to load fixtures")
	}
}

func createSqlStorage(dbType string, storageType sqldb.SQLType, cfg *config) (storage.StorageInterface, error) {
	db, err := sql.Open(dbType, cfg.DbDsn)
	if err != nil {
		return nil, fmt.Errorf("can't open database: %w", err)
	}

	return sqldb.NewStorage(storageType, db, nil)
}

func createAerospikeStorage(cfg *config) (storage.StorageInterface, error) {
	parts := strings.Split(cfg.DbDsn, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("can't parse aerospike host %q, should be in form of host:port/namespace", cfg.DbDsn)
	}
	namespace := parts[1]

	host := parts[0]
	hostParts := strings.Split(host, ":")
	address := hostParts[0]
	port, err := strconv.Atoi(hostParts[1])
	if err != nil {
		return nil, fmt.Errorf("can't parse port: %s", parts[1])
	}

	client, err := aerospike.NewClient(address, port)
	if err != nil {
		return nil, fmt.Errorf("can't connect to aerospike: %w", err)
	}
	return aerospikeStorage.NewStorage(client, namespace, nil), nil
}

func createRedisStorage(cfg *config) (storage.StorageInterface, error) {
	redisOptions, err := redis.ParseURL(cfg.DbDsn)
	if err != nil {
		return nil, fmt.Errorf("can't parse Redis url %q is not a valid URL: %w", cfg.DbDsn, err)
	}

	client := redis.NewClient(redisOptions)
	return redisStorage.NewStorage(client, nil), nil
}

func validateConfig(cfg *config) error {
	if cfg.Host == "" {
		return errors.New("service hostname not provided")
	}
	if !strings.HasPrefix(cfg.Host, "http://") && !strings.HasPrefix(cfg.Host, "https://") {
		cfg.Host = "http://" + cfg.Host
	}
	cfg.Host = strings.TrimRight(cfg.Host, "/")

	if cfg.TestsLocation == "" {
		return errors.New("no tests location provided")
	}
	return nil
}

func getConfig() *config {
	cfg := &config{}

	flag.StringVar(&cfg.Host, "host", "", "target system hostname")
	flag.StringVar(&cfg.TestsLocation, "tests", "", "path to tests file or directory")
	flag.StringVar(&cfg.EnvFile, "env-file", "", "path to env-file")
	flag.StringVar(&cfg.FixturesLocation, "fixtures", "", "path to fixtures directory")
	flag.StringVar(&cfg.FixturesDefaults, "fixtures-defaults", "", "apply fixture with specified name before processing of every test file")
	flag.StringVar(&cfg.DbType, "db-type", "", "type of database/storage (available options: postgres, mysql, sqlite, aerospike, redis)")
	flag.StringVar(&cfg.DbDsn, "db-dsn", "", "DSN for the fixtures database (WARNING: tables mentioned in fixtures will be truncated!)")
	flag.StringVar(&cfg.Mocks, "mocks", "", "comma separated list of registered mocks")
	flag.StringVar(&cfg.MocksDefaults, "mocks-defaults", "", "file with default mock values")
	flag.StringVar(&cfg.MocksPrefix, "mocks-prefix", "GONKEX_MOCK_", "use specified prefix when register environment variables")

	flag.StringVar(&cfg.PreTestCmd, "pre-test-cmd", "", "program to run before start the tests")
	flag.StringVar(&cfg.PreTestWait, "pre-test-wait", "", "delay before start the tests")

	flag.BoolVar(&cfg.Verbose, "v", false, "Verbose output")
	flag.BoolVar(&cfg.Allure, "allure", false, "Make Allure report")

	flag.Parse()

	return cfg
}

func proxyURLFromEnv() (*url.URL, error) {
	if os.Getenv("HTTP_PROXY") != "" {
		httpURL, err := url.Parse(os.Getenv("HTTP_PROXY"))
		if err != nil {
			return nil, err
		}

		return httpURL, nil
	}

	return nil, nil
}

var _ output.ExtendedOutputInterface = (*testCounter)(nil)

type testCounter struct {
	total, failed, skipped, broken int

	showOutput bool
	testsLoc   string
}

func (h *testCounter) BeforeTest(v models.TestInterface) error {
	if v.FirstTestInFile() {
		name := v.GetFileName()
		if len(name) > len(h.testsLoc) {
			shortName, err := filepath.Rel(h.testsLoc, name)
			if err == nil {
				name = shortName
			}
		}
		h.print("\n" + name + " ")
	}
	h.total++
	if v.GetStatus() == models.StatusBroken {
		h.broken++
		h.print("b")
	}
	if v.GetStatus() == models.StatusSkipped {
		h.skipped++
		h.print("s")
	}
	return nil
}

func (h *testCounter) Process(_ models.TestInterface, result *models.Result) error {
	h.print(".")
	if !result.Passed() {
		h.failed++
	}
	return nil
}

func (h *testCounter) ShowResult() {
	_, _ = fmt.Printf("\n\nsuccess %d, failed %d, skipped %d, broken %d, total %d\n",
		h.total-(h.broken+h.failed+h.skipped),
		h.failed,
		h.skipped,
		h.broken,
		h.total,
	)
}

func (h *testCounter) print(c string) {
	if h.showOutput {
		_, _ = fmt.Printf("%s", c)
	}
}

var _ checker.ExtendedCheckerInterface = (*fixtureApplier)(nil)

type fixtureApplier struct {
	storage  storage.StorageInterface
	location string
	defaults string
}

func (f *fixtureApplier) BeforeTest(m models.TestInterface) error {
	if m.FirstTestInFile() {
		return f.storage.LoadFixtures(f.location, []string{f.defaults})
	}
	return nil
}

func (f *fixtureApplier) Check(models.TestInterface, *models.Result) ([]error, error) {
	return nil, nil
}
