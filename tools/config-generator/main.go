/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The make_config tool generates a full Prow config for the Knative project,
// with input from a yaml file with key definitions.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/util/sets"

	"knative.dev/test-infra/pkg/ghutil"
)

const (
	// Manifests generated by ko are indented by 2 spaces.
	baseIndent  = "  "
	templateDir = "templates"

	// ##########################################################
	// ############## prow configuration templates ##############
	// ##########################################################
	// commonHeaderConfig contains common header definitions.
	commonHeaderConfig = "common_header.yaml"
)

var (
	// GitHub orgs that are using knative.dev path alias.
	pathAliasOrgs = sets.NewString("knative", "knative-sandbox")
	// GitHub repos that are not using knative.dev path alias.
	nonPathAliasRepos = sets.NewString("knative/docs")
)

type logFatalfFunc func(string, ...interface{})

// repositoryData contains basic data about each Knative repository.
type repositoryData struct {
	Name                   string
	EnablePerformanceTests bool
	EnableGoCoverage       bool
	GoCoverageThreshold    int
	Processed              bool
}

// prowConfigTemplateData contains basic data about Prow.
type prowConfigTemplateData struct {
	Year              int
	GcsBucket         string
	PresubmitLogsDir  string
	LogsDir           string
	ProwHost          string
	TestGridHost      string
	GubernatorHost    string
	TestGridGcsBucket string
	TideRepos         []string
	ManagedRepos      []string
	ManagedOrgs       []string
	JobConfigPath     string
	CoreConfigPath    string
	PluginConfigPath  string
	TestInfraRepo     string
}

// baseProwJobTemplateData contains basic data about a Prow job.
type baseProwJobTemplateData struct {
	OrgName             string
	RepoName            string
	RepoNameForJob      string
	GcsBucket           string
	GcsLogDir           string
	GcsPresubmitLogDir  string
	RepoURI             string
	RepoBranch          string
	CloneURI            string
	SecurityContext     []string
	SkipBranches        []string
	Branches            []string
	DecorationConfig    []string
	ExtraRefs           []string
	Command             string
	Args                []string
	Env                 []string
	Volumes             []string
	VolumeMounts        []string
	Resources           []string
	ReporterConfig      []string
	JobStatesToReport   []string
	Timeout             int
	AlwaysRun           bool
	Optional            bool
	TestAccount         string
	ServiceAccount      string
	ReleaseGcs          string
	GoCoverageThreshold int
	Image               string
	Labels              []string
	PathAlias           string
	Cluster             string
	NeedsMonitor        bool
	Annotations         []string
}

// ####################################################################################################
// ################ data definitions that are used for the prow config file generation ################
// ####################################################################################################

// outputter is a struct that directs program output and counts the number of write calls.
type outputter struct {
	io.Writer
	count int
}

func newOutputter(writer io.Writer) outputter {
	return outputter{writer, 0}
}

// outputConfig outputs the given line, if not empty, to the output writer (e.g. stdout).
func (o *outputter) outputConfig(line string) {
	if strings.TrimSpace(line) != "" {
		fmt.Fprintln(o, strings.TrimRight(line, " "))
		o.count++
	}
}

// sectionGenerator is a function that generates Prow job configs given a slice of a yaml file with configs.
type sectionGenerator func(string, string, yaml.MapSlice)

// stringArrayFlag is the content of a multi-value flag.
type stringArrayFlag []string

var (
	// Values used in the jobs that can be changed through command-line flags.
	// TODO: these should be CapsCase
	// ... until they are not global
	output                   outputter
	logFatalf                logFatalfFunc
	prowHost                 string
	testGridHost             string
	gubernatorHost           string
	GCSBucket                string
	testGridGcsBucket        string
	LogsDir                  string
	presubmitLogsDir         string
	testAccount              string
	nightlyAccount           string
	releaseAccount           string
	prowTestsDockerImage     string
	presubmitScript          string
	releaseScript            string
	webhookAPICoverageScript string
	upgradeReleaseBranches   bool
	githubTokenPath          string

	// #########################################################################
	// ############## data used for generating prow configuration ##############
	// #########################################################################
	// Array constants used throughout the jobs.
	allPresubmitTests = []string{"--all-tests"}
	releaseNightly    = []string{"--publish", "--tag-release"}
	releaseLocal      = []string{"--nopublish", "--notag-release"}

	// Overrides and behavior changes through command-line flags.
	repositoryOverride string
	jobNameFilter      string
	preCommand         string
	extraEnvVars       stringArrayFlag
	timeoutOverride    int

	// List of Knative repositories.
	// Not guaranteed unique by any value of the struct
	repositories []repositoryData

	// Map which sections of the config.yaml were written to stdout.
	sectionMap map[string]bool

	releaseRegex = regexp.MustCompile(`.+-[0-9\.]+$`)
)

// Yaml parsing helpers.

// read template yaml file content
func readTemplate(fp string) string {
	if _, ok := templatesCache[fp]; !ok {
		// get the directory of the currently running file
		_, f, _, _ := runtime.Caller(0)
		content, err := ioutil.ReadFile(path.Join(path.Dir(f), templateDir, fp))
		if err != nil {
			logFatalf("Failed read file '%s': '%v'", fp, err)
		}
		templatesCache[fp] = string(content)
	}
	return templatesCache[fp]
}

// Config generation functions.

// newbaseProwJobTemplateData returns a baseProwJobTemplateData type with its initial, default values.
func newbaseProwJobTemplateData(repo string) baseProwJobTemplateData {
	var data baseProwJobTemplateData
	data.Timeout = 50
	data.OrgName = strings.Split(repo, "/")[0]
	data.RepoName = strings.Replace(repo, data.OrgName+"/", "", 1)
	data.ExtraRefs = []string{"- org: " + data.OrgName, "  repo: " + data.RepoName}
	if pathAliasOrgs.Has(data.OrgName) && !nonPathAliasRepos.Has(repo) {
		data.PathAlias = "path_alias: knative.dev/" + data.RepoName
		data.ExtraRefs = append(data.ExtraRefs, "  "+data.PathAlias)
	}
	data.RepoNameForJob = strings.ToLower(strings.Replace(repo, "/", "-", -1))

	data.RepoBranch = "main" // Default to be main for other repos
	data.GcsBucket = GCSBucket
	data.RepoURI = "github.com/" + repo
	data.CloneURI = fmt.Sprintf("\"https://%s.git\"", data.RepoURI)
	data.GcsLogDir = fmt.Sprintf("gs://%s/%s", GCSBucket, LogsDir)
	data.GcsPresubmitLogDir = fmt.Sprintf("gs://%s/%s", GCSBucket, presubmitLogsDir)
	data.ReleaseGcs = strings.Replace(repo, data.OrgName+"/", "knative-releases/", 1)
	data.AlwaysRun = true
	data.Optional = false
	data.Image = prowTestsDockerImage
	data.ServiceAccount = testAccount
	data.Command = ""
	data.Args = make([]string, 0)
	data.Volumes = make([]string, 0)
	data.VolumeMounts = make([]string, 0)
	data.Env = make([]string, 0)
	data.Labels = make([]string, 0)
	data.Annotations = make([]string, 0)
	data.Cluster = "cluster: \"build-knative\""
	return data
}

// General helpers.

// createCommand returns an array with the command to run and its arguments.
func createCommand(data baseProwJobTemplateData) []string {
	c := []string{data.Command}
	// Prefix the pre-command if present.
	if preCommand != "" {
		c = append([]string{preCommand}, c...)
	}
	return append(c, data.Args...)
}

func envNameToKey(key string) string {
	return "- name: " + key
}

func envValueToValue(value string) string {
	return "  value: " + value
}

// addEnvToJob adds the given key/pair environment variable to the job.
func (data *baseProwJobTemplateData) addEnvToJob(key, value string) {
	// Value should always be string. Add quotes if we get a number
	if isNum(value) {
		value = "\"" + value + "\""
	}

	data.Env = append(data.Env, envNameToKey(key), envValueToValue(value))
}

// addLabelToJob adds extra labels to a job
func addLabelToJob(data *baseProwJobTemplateData, key, value string) {
	(*data).Labels = append((*data).Labels, []string{key + ": " + value}...)
}

// addPubsubLabelsToJob adds the pubsub labels so the prow job message will be picked up by test-infra monitoring
func addMonitoringPubsubLabelsToJob(data *baseProwJobTemplateData, runID string) {
	addLabelToJob(data, "prow.k8s.io/pubsub.project", "knative-tests")
	addLabelToJob(data, "prow.k8s.io/pubsub.topic", "knative-monitoring")
	addLabelToJob(data, "prow.k8s.io/pubsub.runID", runID)
}

// addVolumeToJob adds the given mount path as volume for the job.
func addVolumeToJob(data *baseProwJobTemplateData, mountPath, name string, isSecret bool, content []string) {
	(*data).VolumeMounts = append((*data).VolumeMounts, []string{"- name: " + name, "  mountPath: " + mountPath}...)
	if isSecret {
		(*data).VolumeMounts = append((*data).VolumeMounts, "  readOnly: true")
	}
	s := []string{"- name: " + name}
	if isSecret {
		arr := []string{"  secret:", "    secretName: " + name}
		s = append(s, arr...)
	}
	for _, line := range content {
		s = append(s, "  "+line)
	}
	(*data).Volumes = append((*data).Volumes, s...)
}

// configureServiceAccountForJob adds the necessary volumes for the service account for the job.
func configureServiceAccountForJob(data *baseProwJobTemplateData) {
	if data.ServiceAccount == "" {
		return
	}
	p := strings.Split(data.ServiceAccount, "/")
	if len(p) != 4 || p[0] != "" || p[1] != "etc" || p[3] != "service-account.json" {
		logFatalf("Service account path %q is expected to be \"/etc/<name>/service-account.json\"", data.ServiceAccount)
	}
	name := p[2]
	addVolumeToJob(data, "/etc/"+name, name, true, nil)
}

// addExtraEnvVarsToJob adds extra environment variables to a job.
func addExtraEnvVarsToJob(envVars []string, data *baseProwJobTemplateData) {
	for _, env := range envVars {
		pair := strings.SplitN(env, "=", 2)
		if len(pair) == 2 {
			data.addEnvToJob(pair[0], pair[1])
		} else {
			logFatalf("Environment variable %q is expected to be \"key=value\"", env)
		}
	}
}

// setupDockerInDockerForJob enables docker-in-docker for the given job.
func setupDockerInDockerForJob(data *baseProwJobTemplateData) {
	// These volumes are required for running docker command and creating kind clusters.
	// Reference: https://github.com/kubernetes-sigs/kind/issues/303
	addVolumeToJob(data, "/docker-graph", "docker-graph", false, []string{"emptyDir: {}"})
	addVolumeToJob(data, "/lib/modules", "modules", false, []string{"hostPath:", "  path: /lib/modules", "  type: Directory"})
	addVolumeToJob(data, "/sys/fs/cgroup", "cgroup", false, []string{"hostPath:", "  path: /sys/fs/cgroup", "  type: Directory"})
	data.addEnvToJob("DOCKER_IN_DOCKER_ENABLED", "\"true\"")
	(*data).SecurityContext = []string{"privileged: true"}
}

// setResourcesReqForJob sets resource requirement for job
func setResourcesReqForJob(res yaml.MapSlice, data *baseProwJobTemplateData) {
	data.Resources = nil
	for _, val := range res {
		data.Resources = append(data.Resources, fmt.Sprintf("  %s:", getString(val.Key)))
		for _, item := range getMapSlice(val.Value) {
			data.Resources = append(data.Resources, fmt.Sprintf("    %s: %s", getString(item.Key), getString(item.Value)))
		}
	}
}

// setReporterConfigReqForJob sets reporter requirement for job
func setReporterConfigReqForJob(res yaml.MapSlice, data *baseProwJobTemplateData) {
	data.ReporterConfig = nil
	for _, val := range res {
		data.ReporterConfig = append(data.ReporterConfig, fmt.Sprintf("  %s:", getString(val.Key)))
		for _, item := range getMapSlice(val.Value) {
			if arr, ok := item.Value.([]interface{}); ok {
				data.JobStatesToReport = getStringArray(arr)
			} else {
				data.ReporterConfig = append(data.ReporterConfig, fmt.Sprintf("    %s: %s", getString(item.Key), getString(item.Value)))
			}
		}
	}
}

// Config parsers.

// parseBasicJobConfigOverrides updates the given baseProwJobTemplateData with any base option present in the given config.
func parseBasicJobConfigOverrides(data *baseProwJobTemplateData, config yaml.MapSlice) {
	(*data).ExtraRefs = append((*data).ExtraRefs, "  base_ref: "+(*data).RepoBranch)
	for i, item := range config {
		switch item.Key {
		case "skip_branches":
			(*data).SkipBranches = getStringArray(item.Value)
		case "branches":
			(*data).Branches = getStringArray(item.Value)
		case "args":
			(*data).Args = getStringArray(item.Value)
		case "timeout":
			(*data).Timeout = getInt(item.Value)
		case "command":
			(*data).Command = getString(item.Value)
		case "needs-monitor":
			(*data).NeedsMonitor = getBool(item.Value)
		case "needs-dind":
			if getBool(item.Value) {
				setupDockerInDockerForJob(data)
			}
		case "always-run":
			(*data).AlwaysRun = getBool(item.Value)
		case "performance":
			for i, repo := range repositories {
				if path.Base(repo.Name) == (*data).RepoName {
					repositories[i].EnablePerformanceTests = getBool(item.Value)
				}
			}
		case "env-vars":
			addExtraEnvVarsToJob(getStringArray(item.Value), data)
		case "optional":
			(*data).Optional = getBool(item.Value)
		case "resources":
			setResourcesReqForJob(getMapSlice(item.Value), data)
		case "reporter_config":
			setReporterConfigReqForJob(getMapSlice(item.Value), data)
		case nil: // already processed
			continue
		default:
			logFatalf("Unknown entry %q for job", item.Key)
		}
		// Knock-out the item, signalling it was already parsed.
		config[i] = yaml.MapItem{}
	}

	// Override any values if provided by command-line flags.
	if timeoutOverride > 0 {
		(*data).Timeout = timeoutOverride
	}
}

// getProwConfigData gets some basic, general data for the Prow config.
func getProwConfigData(config yaml.MapSlice) prowConfigTemplateData {
	var data prowConfigTemplateData
	data.Year = time.Now().Year()
	data.ProwHost = prowHost
	data.TestGridHost = testGridHost
	data.GubernatorHost = gubernatorHost
	data.GcsBucket = GCSBucket
	data.TestGridGcsBucket = testGridGcsBucket
	data.PresubmitLogsDir = presubmitLogsDir
	data.LogsDir = LogsDir
	data.TideRepos = make([]string, 0)
	data.ManagedRepos = make([]string, 0)
	data.ManagedOrgs = make([]string, 0)
	// Repos enabled for tide are all those that have presubmit jobs.
	for _, section := range config {
		if section.Key != "presubmits" {
			continue
		}
		for _, repo := range getMapSlice(section.Value) {
			orgRepoName := getString(repo.Key)
			data.TideRepos = appendIfUnique(data.TideRepos, orgRepoName)
			if strings.HasSuffix(orgRepoName, "test-infra") {
				data.TestInfraRepo = orgRepoName
			}
		}
	}

	// Sort repos to make output stable.
	sort.Strings(data.TideRepos)
	sort.Strings(data.ManagedOrgs)
	sort.Strings(data.ManagedRepos)
	return data
}

// parseSection generate the configs from a given section of the input yaml file.
func parseSection(config yaml.MapSlice, title string, generate sectionGenerator, finalize sectionGenerator) {
	for _, section := range config {
		if section.Key != title {
			continue
		}
		for _, repo := range getMapSlice(section.Value) {
			repoName := getString(repo.Key)
			for _, jobConfig := range getInterfaceArray(repo.Value) {
				generate(title, repoName, getMapSlice(jobConfig))
			}
			if finalize != nil {
				finalize(title, repoName, nil)
			}
		}
	}
}

// Template helpers.

// gitHubRepo returns the correct reference for the GitHub repository.
func gitHubRepo(data baseProwJobTemplateData) string {
	if repositoryOverride != "" {
		return repositoryOverride
	}
	s := data.RepoURI
	if data.RepoBranch != "" {
		s += "=" + data.RepoBranch
	}
	return s
}

// executeTemplate outputs the given job template with the given data, respecting any filtering.
func executeJobTemplate(name, templ, title, repoName, jobName string, groupByRepo bool, data interface{}) {
	if jobNameFilter != "" && jobNameFilter != jobName {
		return
	}
	if !sectionMap[title] {
		output.outputConfig(title + ":")
		sectionMap[title] = true
	}
	if groupByRepo {
		if !sectionMap[title+repoName] {
			output.outputConfig(baseIndent + repoName + ":")
			sectionMap[title+repoName] = true
		}
	}
	executeTemplate(name, templ, data)
}

// executeTemplate outputs the given template with the given data.
func executeTemplate(name, templ string, data interface{}) {
	var res bytes.Buffer
	funcMap := template.FuncMap{
		"indent_section":       indentSection,
		"indent_array_section": indentArraySection,
		"indent_array":         indentArray,
		"indent_keys":          indentKeys,
		"indent_map":           indentMap,
		"repo":                 gitHubRepo,
	}
	t := template.Must(template.New(name).Funcs(funcMap).Delims("[[", "]]").Parse(templ))
	if err := t.Execute(&res, data); err != nil {
		logFatalf("Error in template %s: %v", name, err)
	}
	for _, line := range strings.Split(res.String(), "\n") {
		output.outputConfig(line)
	}
}

// Multi-value flag parser.

func (a *stringArrayFlag) String() string {
	return strings.Join(*a, ", ")
}

func (a *stringArrayFlag) Set(value string) error {
	*a = append(*a, value)
	return nil
}

// parseJob gets the job data from the original yaml data, now the jobName can be "presubmits" or "periodic"
func parseJob(config yaml.MapSlice, jobName string) yaml.MapSlice {
	for _, section := range config {
		if section.Key == jobName {
			return getMapSlice(section.Value)
		}
	}

	logFatalf("The metadata misses %s configuration, cannot continue.", jobName)
	return nil
}

// parseGoCoverageMap constructs a map, indicating which repo is enabled for go coverage check
func parseGoCoverageMap(presubmitJob yaml.MapSlice) map[string]bool {
	goCoverageMap := make(map[string]bool)
	for _, repo := range presubmitJob {
		repoName := strings.Split(getString(repo.Key), "/")[1]
		goCoverageMap[repoName] = false
		for _, jobConfig := range getInterfaceArray(repo.Value) {
			for _, item := range getMapSlice(jobConfig) {
				if item.Key == "go-coverage" {
					goCoverageMap[repoName] = getBool(item.Value)
					break
				}
			}
		}
	}

	return goCoverageMap
}

// collectMetaData collects the meta data from the original yaml data, which can be then used for building the test groups and dashboards config
func collectMetaData(periodicJob yaml.MapSlice) {
	for _, repo := range periodicJob {
		rawName := getString(repo.Key)
		projName := strings.Split(rawName, "/")[0]
		repoName := strings.Split(rawName, "/")[1]
		jobDetailMap := metaData.Get(projName)
		metaData.EnsureRepo(projName, repoName)

		// parse job configs
		for _, conf := range getInterfaceArray(repo.Value) {
			jobDetailMap = metaData.Get(projName)
			jobConfig := getMapSlice(conf)
			enabled := false
			jobName := ""
			releaseVersion := ""
			for _, item := range jobConfig {
				switch item.Key {
				case "continuous", "dot-release", "auto-release", "performance",
					"nightly", "webhook-apicoverage":
					if getBool(item.Value) {
						enabled = true
						jobName = getString(item.Key)
					}
				case "branch-ci":
					enabled = getBool(item.Value)
					jobName = "continuous"
				case "release":
					releaseVersion = getString(item.Value)
				case "custom-job":
					enabled = true
					jobName = getString(item.Value)
				default:
					// continue here since we do not need to care about other entries, like cron, command, etc.
					continue
				}
			}
			// add job types for the corresponding repos, if needed
			if enabled {
				// if it's a job for a release branch
				if releaseVersion != "" {
					releaseProjName := fmt.Sprintf("%s-%s", projName, releaseVersion)

					// TODO: Why do we assign?
					jobDetailMap = metaData.Get(releaseProjName)
				}
				jobDetailMap.Add(repoName, jobName)
			}
		}
		updateTestCoverageJobDataIfNeeded(jobDetailMap, repoName)
	}

	// add test coverage jobs for the repos that haven't been handled
	addRemainingTestCoverageJobs()
}

// updateTestCoverageJobDataIfNeeded adds test-coverage job data for the repo if it has go coverage check
func updateTestCoverageJobDataIfNeeded(jobDetailMap JobDetailMap, repoName string) {
	if goCoverageMap[repoName] {
		jobDetailMap.Add(repoName, "test-coverage")
		// delete this repoName from the goCoverageMap to avoid it being processed again when we
		// call the function addRemainingTestCoverageJobs
		delete(goCoverageMap, repoName)
	}
}

// addRemainingTestCoverageJobs adds test-coverage jobs data for the repos that haven't been processed.
func addRemainingTestCoverageJobs() {
	// handle repos that only have go coverage
	for repoName, hasGoCoverage := range goCoverageMap {
		if hasGoCoverage {
			jobDetailMap := metaData.Get(metaData.projNames[0]) // TODO: WTF why projNames[0] !??!?!?!?
			jobDetailMap.Add(repoName, "test-coverage")
		}
	}
}

// buildProjRepoStr builds the projRepoStr used in the config file with projName and repoName
func buildProjRepoStr(projName string, repoName string) string {
	projVersion := ""
	if releaseRegex.MatchString(projName) {
		projNameAndVersion := strings.Split(projName, "-")
		// The project name can possibly contain "-" as well, so we need to consider the last part as the version,
		// and the rest be the project name.
		// For example, "knative-sandbox-0.15" will be split into "knative-sandbox" and "0.15"
		projVersion = projNameAndVersion[len(projNameAndVersion)-1]
		projName = strings.TrimRight(projName, "-"+projVersion)
	}
	projRepoStr := repoName
	if projVersion != "" {
		projRepoStr += "-" + projVersion
	}
	projRepoStr = projName + "-" + projRepoStr
	return strings.ToLower(projRepoStr)
}

// isReleased returns true for project name that has version
func isReleased(projName string) bool {
	return releaseRegex.FindString(projName) != ""
}

// setOutput set the given file as the output target, then all the output will be written to this file
func setOutput(fileName string) {
	output = newOutputter(os.Stdout)
	if fileName == "" {
		return
	}
	configFile, err := os.OpenFile(fileName, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		logFatalf("Cannot create the configuration file %q: %v", fileName, err)
		return
	}
	configFile.Truncate(0)
	configFile.Seek(0, 0)
	output = newOutputter(configFile)
}

// main is the script entry point.
func main() {
	logFatalf = log.Fatalf
	// Parse flags and check them.
	prowJobsConfigOutput := ""
	testgridConfigOutput := ""
	k8sTestgridConfigOutput := ""
	var generateTestgridConfig = flag.Bool("generate-testgrid-config", true, "Whether to generate the testgrid config from the template file")
	var generateK8sTestgridConfig = flag.Bool("generate-k8s-testgrid-config", true, "Whether to generate the k8s testgrid config from the template file")
	var includeConfig = flag.Bool("include-config", true, "Whether to include general configuration (e.g., plank) in the generated config")
	var dockerImagesBase = flag.String("image-docker", "gcr.io/knative-tests/test-infra", "Default registry for the docker images used by the jobs")
	flag.StringVar(&prowJobsConfigOutput, "prow-jobs-config-output", "", "The destination for the prow jobs config output, default to be stdout")
	flag.StringVar(&testgridConfigOutput, "testgrid-config-output", "", "The destination for the testgrid config output, default to be stdout")
	flag.StringVar(&k8sTestgridConfigOutput, "k8s-testgrid-config-output", "", "The destination for the k8s testgrid config output, default to be stdout")
	flag.StringVar(&prowHost, "prow-host", "https://prow.knative.dev", "Prow host, including HTTP protocol")
	flag.StringVar(&testGridHost, "testgrid-host", "https://testgrid.knative.dev", "TestGrid host, including HTTP protocol")
	flag.StringVar(&gubernatorHost, "gubernator-host", "https://gubernator.knative.dev", "Gubernator host, including HTTP protocol")
	flag.StringVar(&GCSBucket, "gcs-bucket", "knative-prow", "GCS bucket to upload the logs to")
	flag.StringVar(&testGridGcsBucket, "testgrid-gcs-bucket", "knative-testgrid", "TestGrid GCS bucket")
	flag.StringVar(&LogsDir, "logs-dir", "logs", "Path in the GCS bucket to upload logs of periodic and post-submit jobs")
	flag.StringVar(&presubmitLogsDir, "presubmit-logs-dir", "pr-logs", "Path in the GCS bucket to upload logs of pre-submit jobs")
	flag.StringVar(&testAccount, "test-account", "/etc/test-account/service-account.json", "Path to the service account JSON for test jobs")
	flag.StringVar(&nightlyAccount, "nightly-account", "/etc/nightly-account/service-account.json", "Path to the service account JSON for nightly release jobs")
	flag.StringVar(&releaseAccount, "release-account", "/etc/release-account/service-account.json", "Path to the service account JSON for release jobs")
	var prowTestsDockerImageName = flag.String("prow-tests-docker", "prow-tests:stable", "prow-tests docker image")
	flag.StringVar(&presubmitScript, "presubmit-script", "./test/presubmit-tests.sh", "Executable for running presubmit tests")
	flag.StringVar(&releaseScript, "release-script", "./hack/release.sh", "Executable for creating releases")
	flag.StringVar(&webhookAPICoverageScript, "webhook-api-coverage-script", "./test/apicoverage.sh", "Executable for running webhook apicoverage tool")
	flag.StringVar(&repositoryOverride, "repo-override", "", "Repository path (github.com/foo/bar[=branch]) to use instead for a job")
	flag.IntVar(&timeoutOverride, "timeout-override", 0, "Timeout (in minutes) to use instead for a job")
	flag.StringVar(&jobNameFilter, "job-filter", "", "Generate only this job, instead of all jobs")
	flag.StringVar(&preCommand, "pre-command", "", "Executable for running instead of the real command of a job")
	flag.BoolVar(&upgradeReleaseBranches, "upgrade-release-branches", false, "Update release branches jobs based on active branches")
	flag.StringVar(&githubTokenPath, "github-token-path", "", "Token path for authenticating with github, used only when --upgrade-release-branches is on")
	flag.Var(&extraEnvVars, "extra-env", "Extra environment variables (key=value) to add to a job")
	flag.Parse()
	if len(flag.Args()) != 1 {
		log.Fatal("Pass the config file as parameter")
	}

	prowTestsDockerImage = path.Join(*dockerImagesBase, *prowTestsDockerImageName)

	// We use MapSlice instead of maps to keep key order and create predictable output.
	configYaml := yaml.MapSlice{}

	// Read input config.
	configFileName := flag.Arg(0)
	if upgradeReleaseBranches {
		gc, err := ghutil.NewGithubClient(githubTokenPath)
		if err != nil {
			logFatalf("Failed creating github client from %q: %v", githubTokenPath, err)
		}
		if err := upgradeReleaseBranchesTemplate(configFileName, gc); err != nil {
			logFatalf("Failed upgrade based on release branch: '%v'", err)
		}
	}

	configFileContent, err := ioutil.ReadFile(configFileName)
	if err != nil {
		logFatalf("Cannot read file %q: %v", configFileName, err)
	}
	if err = yaml.Unmarshal(configFileContent, &configYaml); err != nil {
		logFatalf("Cannot parse config %q: %v", configFileName, err)
	}

	prowConfigData := getProwConfigData(configYaml)

	// Generate Prow config.
	repositories = make([]repositoryData, 0)
	sectionMap = make(map[string]bool)
	setOutput(prowJobsConfigOutput)
	executeTemplate("general header", readTemplate(commonHeaderConfig), prowConfigData)
	parseSection(configYaml, "presubmits", generatePresubmit, nil)
	parseSection(configYaml, "periodics", generatePeriodic, generateGoCoveragePeriodic)
	for _, repo := range repositories { // Keep order for predictable output.
		if !repo.Processed && repo.EnableGoCoverage {
			generateGoCoveragePeriodic("periodics", repo.Name, nil)
		}
	}
	generatePerfClusterUpdatePeriodicJobs()

	for _, repo := range repositories {
		if repo.EnableGoCoverage {
			generateGoCoveragePostsubmit("postsubmits", repo.Name, nil)
		}
		if repo.EnablePerformanceTests {
			generatePerfClusterPostsubmitJob(repo)
		}
	}

	// config object is modified when we generate prow config, so we'll need to reload it here
	if err = yaml.Unmarshal(configFileContent, &configYaml); err != nil {
		logFatalf("Cannot parse config %q: %v", configFileName, err)
	}

	if *generateK8sTestgridConfig {
		setOutput(k8sTestgridConfigOutput)
		executeTemplate("general header", readTemplate(commonHeaderConfig), newBaseTestgridTemplateData(""))

		periodicJobData := parseJob(configYaml, "periodics")
		orgsAndRepoSet := make(map[string]sets.String)

		// All periodics should be included in Testgrid.
		for _, mapItem := range periodicJobData {
			org, repo := parseOrgAndRepoFromMapItem(mapItem)
			if _, exists := orgsAndRepoSet[org]; !exists {
				orgsAndRepoSet[org] = sets.NewString()
			}
			orgsAndRepoSet[org].Insert(repo)
		}

		// Do a special insert for the beta prow test jobs.
		orgsAndRepoSet["knative"].Insert("prow-tests")

		orgsAndRepos := make(map[string][]string)
		for org, repoSet := range orgsAndRepoSet {
			orgsAndRepos[org] = repoSet.List()
		}
		generateK8sTestgrid(orgsAndRepos)
	}

	// Generate Testgrid config.
	if *generateTestgridConfig {
		setOutput(testgridConfigOutput)

		if *includeConfig {
			executeTemplate("general header", readTemplate(commonHeaderConfig), newBaseTestgridTemplateData(""))
			executeTemplate("general config", readTemplate(generalTestgridConfig), newBaseTestgridTemplateData(""))
		}

		presubmitJobData := parseJob(configYaml, "presubmits")
		goCoverageMap = parseGoCoverageMap(presubmitJobData)

		periodicJobData := parseJob(configYaml, "periodics")
		collectMetaData(periodicJobData)
		addCustomJobsTestgrid()

		// log.Print(spew.Sdump(metaData))

		// These generate "test_groups:"
		metaData.generateTestGridSection("test_groups", generateTestGroup, false)
		metaData.generateNonAlignedTestGroups()

		// These generate "dashboards:"
		metaData.generateTestGridSection("dashboards", generateDashboard, true)
		metaData.generateDashboardsForReleases()
		metaData.generateNonAlignedDashboards()

		// These generate "dashboard_groups:"
		metaData.generateDashboardGroups()
		metaData.generateNonAlignedDashboardGroups()
	}
}

// parseOrgAndRepoFromMapItem splits the "org/repo" string of a yaml.MapItem
// into "org" and "repo" return values.
func parseOrgAndRepoFromMapItem(mapItem yaml.MapItem) (string, string) {
	orgAndRepo := strings.Split(mapItem.Key.(string), "/")
	org := orgAndRepo[0]
	repo := orgAndRepo[1]
	return org, repo
}
