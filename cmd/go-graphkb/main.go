package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/clems4ever/go-graphkb/internal/database"
	"github.com/clems4ever/go-graphkb/internal/history"
	"github.com/clems4ever/go-graphkb/internal/knowledge"
	"github.com/clems4ever/go-graphkb/internal/server"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Database the selected database
var Database *database.MariaDB

// Historizer handles the query history
var Historizer history.Historizer

// ConfigPath string
var ConfigPath string

// LogLevel the log level
var LogLevel string

func main() {
	// Also read env variables prefixed with GRAPHKB_.
	viper.SetEnvPrefix("GRAPHKB")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	rootCmd := &cobra.Command{
		Use: "go-graphkb [opts]",
	}

	listenCmd := &cobra.Command{
		Use: "listen",
		Run: listen,
	}

	cleanCmd := &cobra.Command{
		Use: "count",
		Run: count,
	}

	countCmd := &cobra.Command{
		Use: "flush",
		Run: flush,
	}

	readCmd := &cobra.Command{
		Use:  "read [source]",
		Run:  read,
		Args: cobra.ExactArgs(1),
	}

	queryCmd := &cobra.Command{
		Use:  "query [query]",
		Run:  queryFunc,
		Args: cobra.ExactArgs(1),
	}

	rootCmd.PersistentFlags().StringVar(&ConfigPath, "config", "config.yml", "Provide the path to the configuration file (required)")
	rootCmd.PersistentFlags().StringVar(&LogLevel, "log-level", "info", "The log level among 'debug', 'info', 'warn', 'error'")

	cobra.OnInitialize(onInit)

	rootCmd.AddCommand(cleanCmd, listenCmd, countCmd, readCmd, queryCmd)
	if err := rootCmd.Execute(); err != nil {
		logrus.Fatal(err)
	}
}

func logLevelParamToSeverity(level string) logrus.Level {
	switch level {
	case "debug":
		return logrus.DebugLevel
	case "info":
		return logrus.InfoLevel
	case "warn":
		return logrus.WarnLevel
	case "error":
		return logrus.ErrorLevel
	}
	logrus.Fatalf("Provided level %s is not a valid option", level)
	// This should never be reached but needed by the compiler
	return logrus.InfoLevel
}

func onInit() {
	viper.SetConfigFile(ConfigPath)
	viper.SetConfigType("yaml")
	viper.SetDefault("mariadb_max_idle_conns", 10)
	viper.SetDefault("mariadb_max_open_conns", 10)

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			fmt.Printf("no configuration file %q, skipping and using env variable", ConfigPath)
		} else {
			panic(fmt.Errorf("cannot read configuration file from %s", ConfigPath))
		}
	}

	logrus.Info("Using config file: ", viper.ConfigFileUsed())
	logrus.SetLevel(logLevelParamToSeverity(LogLevel))
	logrus.Info("Using log severity: ", LogLevel)

	dbName := viper.GetString("mariadb_database")
	if dbName == "" {
		logrus.Fatal("Please provide database_name option in your configuration file")
	}

	Database = database.NewMariaDB(database.MariaDBConfig{
		Username:               viper.GetString("mariadb_username"),
		Password:               viper.GetString("mariadb_password"),
		Host:                   viper.GetString("mariadb_host"),
		DatabaseName:           dbName,
		AllowCleartextPassword: viper.GetBool("mariadb_allow_cleartext_password"),
		MaxIdleConns:           viper.GetInt("mariadb_max_idle_conns"),
		MaxOpenConns:           viper.GetInt("mariadb_max_open_conns"),
	})

	Historizer = Database
	if viper.GetBool("no_query_history") {
		Historizer = &history.NoopHistorizer{}
	}
}

func count(cmd *cobra.Command, args []string) {
	countAssets, err := Database.CountAssets(context.Background())
	if err != nil {
		logrus.Fatal(err)
	}

	countRelations, err := Database.CountRelations(context.Background())
	if err != nil {
		logrus.Fatal(err)
	}
	fmt.Printf("%d assets\n%d relations\n", countAssets, countRelations)
}

func flush(cmd *cobra.Command, args []string) {
	if err := Database.FlushAll(context.Background()); err != nil {
		logrus.Fatal(err)
	}
	logrus.Info("Successul flush")
}

func listen(cmd *cobra.Command, args []string) {
	if err := Database.InitializeSchema(); err != nil {
		logrus.Fatal(err)
	}

	Database.Start()

	listenInterface := viper.GetString("server_listen")
	concurrency := viper.GetInt64("concurrency")
	if concurrency == 0 {
		concurrency = 32
	}

	server.StartServer(listenInterface, Database, Database, Database, Historizer, concurrency)
}

func read(cmd *cobra.Command, args []string) {
	g := knowledge.NewGraph()
	buff := bytes.NewBuffer(nil)

	encoder := knowledge.NewGraphEncoder(buff)
	err := Database.ReadGraph(context.Background(), args[0], encoder)
	if err != nil {
		logrus.Fatal(err)
	}

	knowledge.NewGraphDecoder(buff).Decode(g)
	fmt.Printf("assets = %d\nrelations = %d\n", len(g.Assets()), len(g.Relations()))
}

func queryFunc(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	q := knowledge.NewQuerier(Database, Database)

	r, err := q.Query(ctx, args[0])
	if err != nil {
		logrus.Fatal(err)
	}

	resultsCount := 0
	for r.Cursor.HasMore() {
		var m interface{}
		err := r.Cursor.Read(context.Background(), &m)
		if err != nil {
			logrus.Fatal(err)
		}

		doc := m.([]interface{})
		ldoc := make([]string, len(doc))
		for i, d := range doc {
			ldoc[i] = fmt.Sprintf("%v", d)
		}
		fmt.Println(ldoc)
		resultsCount++
	}

	totalTime := r.Statistics.Parsing + r.Statistics.Execution

	fmt.Printf("%d results found in %fms\n", resultsCount, float64(totalTime.Microseconds())/1000.0)
}
