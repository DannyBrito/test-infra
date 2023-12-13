package main

import (
	"bytes"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed test.template.yml
var testTemplateFS embed.FS

const (
	GCSLOGPREFIX = "kubernetes-jenkins/logs/"
	COMMENT      = "AUTO-GENERATED by releng/generate-tests/main.go - DO NOT EDIT."
)

type options struct {
	yamlConfigPath     string
	testgridOutputPath string
	outputDir          string
}

func parseFlags() *options {
	opt := options{}
	flag.StringVar(&opt.outputDir, "output-dir", "config/jobs/kubernetes/generated/", "Write configmap here instead of stdout")
	flag.StringVar(&opt.testgridOutputPath, "testgrid-output-path", "config/testgrids/generated-test-config.yaml", "Name of resource")
	flag.StringVar(&opt.yamlConfigPath, "yaml-config-path", "", "Namespace for resource")
	flag.Parse()
	return &opt
}

func (opt *options) getYamlConfig() ConfigFile {
	yamlFile, err := os.ReadFile(opt.yamlConfigPath)
	if err != nil {
		log.Fatalln("error trying to read yaml config path file")
	}
	var config ConfigFile
	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		log.Fatalln("error trying to parse yaml config path file")
	}
	return config
}

func (opt *options) validateOptions() error {
	if opt.outputDir == "" {
		return errors.New("--output-dir must be specified")
	}
	if opt.testgridOutputPath == "" {
		return errors.New("--testgrid-output-path must be specified")
	}
	if opt.yamlConfigPath == "" {
		return errors.New("--yaml-config-path must be specified")
	}
	return nil
}

func main() {
	options := parseFlags()
	if err := options.validateOptions(); err != nil {
		log.Fatalln(err)
	}
	config := options.getYamlConfig()
	var jobNames []string
	for name := range config.Jobs {
		jobNames = append(jobNames, name)
	}
	slices.Sort(jobNames)
	outputConfig := ProwConfigFile{
		Periodics: []Periodic{},
	}
	testgridConfig := TestgridConfig{
		TestGroups: []TestGroup{},
	}
	for _, jobName := range jobNames {
		prow, testgrid := forEachJob(options.outputDir, jobName, config.Jobs[jobName], config)
		outputConfig.Periodics = append(outputConfig.Periodics, prow)
		if !testgrid.isEmpty() {
			testgridConfig.TestGroups = append(testgridConfig.TestGroups, testgrid)
		}
	}
	prowfilePath := filepath.Join(options.outputDir, "generated.yaml")
	// writeConfigToFile(prowfilePath, outputConfig, "")
	SaveConfigsToFile(outputConfig, prowfilePath)
	// writeConfigToFile(options.testgridOutputPath, testgridConfig, COMMENT)
}

func writeConfigToFile(outputFile string, config interface{}, comment string) {
	fmt.Printf("writing configuration to: %s\n", outputFile)

	data, err := yaml.Marshal(&config)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}

	if comment != "" {
		comment = "# " + comment + "\n\n"
		data = append([]byte(comment), data...)
	}

	err = os.WriteFile(outputFile, data, 0644)
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func SaveConfigsToFile(data interface{}, outputFilePath string) {
	tmpt, err := template.ParseFS(testTemplateFS, "test.template.yml")
	if err != nil {
		log.Fatalf("fail to Parse ConfigFile Template: , %+v", err)
	}
	var buf bytes.Buffer
	err = tmpt.Execute(&buf, data)
	if err != nil {
		log.Fatalf("fail to render configs struct to yaml template, %+v", err)
	}
	log.Println("writing result output config file")
	if err := os.WriteFile(outputFilePath, buf.Bytes(), 0o600); err != nil {
		log.Fatalf("fail to write configs struct to yaml, %s", err)
	}
}

func forEachJob(outputDir string, jobName string, job Job, config ConfigFile) (Periodic, TestGroup) {
	var jobConfig Job
	var prowConfig Periodic
	var testgridConfig TestGroup
	fields := strings.Split(jobName, "-")
	if len(fields) < 3 {
		log.Fatalln("Expected at least 3 fields in job name", jobName)
	}
	jobType := fields[2]
	switch jobType {
	case "e2e":
		e2eTest := newE2ETest(outputDir, jobName, job, config)
		jobConfig, prowConfig, testgridConfig = e2eTest.generate()
	case "e2enode":
		e2eNodeTest := newE2ENodeTest(jobName, job, config)
		jobConfig, prowConfig = e2eNodeTest.generate()
	default:
		log.Fatalf("Job %s has unexpected job type %s", jobName, jobType)
	}
	jobConfig.Args = applyJobOverrides(jobConfig.Args, getArgs(jobName, job.Args))
	prowConfig.Spec.Containers[0].Args = append(prowConfig.Spec.Containers[0].Args, jobConfig.Args...)
	file := fmt.Sprintf("/workspace/scenarios/%s.py", jobConfig.Scenario)
	prowConfig.Spec.Containers[0].Command = []string{"runner.sh", file}
	return prowConfig, testgridConfig
}

func applyJobOverrides(envsOrArgs []string, jobEnvsOrArgs []string) []string {
	originalEnvsOrArgs := append([]string(nil), envsOrArgs...)
	for _, jobEnvOrArg := range jobEnvsOrArgs {
		name := strings.Split(jobEnvOrArg, "=")[0]
		var envOrArg string
		for _, val := range originalEnvsOrArgs {
			trimVal := strings.TrimSpace(val)
			if strings.HasPrefix(trimVal, name+"=") || trimVal == name {
				envOrArg = val
				break
			}
		}
		if envOrArg != "" {
			for i, v := range envsOrArgs {
				if v == envOrArg {
					envsOrArgs = append(envsOrArgs[:i], envsOrArgs[i+1:]...)
					break
				}
			}
		}
		envsOrArgs = append(envsOrArgs, jobEnvOrArg)
	}
	return envsOrArgs
}

func getSHA1Hash(data string) string {
	h := sha1.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func substitute(jobName string, lines []string) []string {
	var result []string
	for _, line := range lines {
		result = append(result, strings.Replace(line, "${job_name_hash}", getSHA1Hash(jobName)[:10], -1))
	}
	return result
}

func getArgs(jobName string, args []string) []string {
	return substitute(jobName, args)
}

func newE2ETest(outputDir string, jobName string, job Job, config ConfigFile) E2ETest {
	envFilePath := filepath.Join(outputDir, jobName+".env")
	fields := strings.Split(jobName, "-")
	if len(fields) != 7 {
		log.Fatalln("Expected 7 fields in job name", jobName)
	}
	return E2ETest{
		EnvFilename:   envFilePath,
		JobName:       jobName,
		Job:           job,
		fields:        fields,
		Common:        config.Common,
		CloudProvider: config.CloudProviders[fields[3]],
		Image:         config.Images[fields[4]],
		K8SVersion:    config.K8SVersions[fields[5][3:]],
		TestSuite:     config.TestSuites[fields[6]],
	}
}

func (et *E2ETest) generate() (Job, Periodic, TestGroup) {
	log.Printf("generating e2e job: %s", et.JobName)
	if len(et.fields) != 7 {
		log.Fatalln("Expected 7 fields in job name", et.JobName)
	}
	image := et.Image
	cloudProvider := et.CloudProvider
	K8SVersion := et.K8SVersion
	testSuite := et.TestSuite
	args := []string{}
	args = append(args, getArgs(et.JobName, et.Common.Args)...)
	args = append(args, getArgs(et.JobName, cloudProvider.Args)...)
	args = append(args, getArgs(et.JobName, image.Args)...)
	args = append(args, getArgs(et.JobName, K8SVersion.Args)...)
	args = append(args, getArgs(et.JobName, testSuite.Args)...)

	jobConfig := et.getJobDefinition(args)
	prowConfig := et.getProwConfig(testSuite)
	tgConfig := et.getTestGridConfig()
	tabName := fmt.Sprintf("%s-%s-%s-%s", et.fields[3], et.fields[4], et.fields[5], et.fields[6])
	if prowConfig.Annotations == nil {
		prowConfig.Annotations = map[string]string{}
	}
	prowConfig.Annotations["testgrid-tab-name"] = tabName
	dashboards := et.InitializeDashBoardsWithReleaseBlockingInfo(K8SVersion.Version)
	if image.TestgridPrefix != "" {
		dashboard := fmt.Sprintf("%s-%s-%s", image.TestgridPrefix, et.fields[4], et.fields[5])
		dashboards = append(dashboards, dashboard)
	}
	prowConfig.Annotations["testgrid-dashboards"] = strings.Join(dashboards, ", ")
	prowConfig.Annotations["testgrid-num-failures-to-alert"] = strconv.Itoa(et.Job.TestgridNumFailuresToAlert)
	return jobConfig, prowConfig, tgConfig
}

func (et *E2ETest) InitializeDashBoardsWithReleaseBlockingInfo(version string) []string {
	dashboards := []string{}
	dashboard := "sig-release-generated"
	if et.Job.ReleaseBlocking {
		dashboard = fmt.Sprintf("sig-release-%s-blocking", version)
	} else if et.Job.ReleaseInforming {
		dashboard = fmt.Sprintf("sig-release-%s-informing", version)
	}
	dashboards = append(dashboards, dashboard)
	return dashboards
}

func (et *E2ETest) getJobDefinition(args []string) Job {
	rSigOwner := et.Job.SigOwners
	if len(rSigOwner) == 0 {
		rSigOwner = []string{"UNKOWN"}
	}

	return Job{
		Scenario:  "kubernetes_e2e",
		Args:      args,
		SigOwners: rSigOwner,
	}
}

func (et *E2ETest) getTestGridConfig() TestGroup {
	return TestGroup{
		Name:      et.JobName,
		GCSPrefix: GCSLOGPREFIX + et.JobName,
		ColumnHeader: []ConfigurationValue{
			{
				ConfigurationValue: "node_os_image",
			},
			{
				ConfigurationValue: "master_os_image",
			},
			{
				ConfigurationValue: "Commit",
			},
			{
				ConfigurationValue: "infra-commit",
			},
		},
	}
}

func (et *E2ETest) getProwConfig(testSuite TestSuite) Periodic {
	prowConfig := Periodic{
		Name: et.JobName,
		Tags: []string{"generated"},
		Labels: map[string]string{
			"preset-service-account": "true",
			"preset-k8s-ssh":         "true",
		},
		Decorate: true,
		DecorationConfig: DecorationConfig{
			Timeout: "180m",
		},
		Spec: Spec{
			Containers: []Container{
				{
					Image: "gcr.io/k8s-staging-test-infra/kubekins-e2e:v20231206-f7b83ffbe6-master",
					Resources: Resources{
						Requests: ComputeResources{
							CPU:    "1000m",
							Memory: "3Gi",
						},
						Limits: ComputeResources{
							CPU:    "1000m",
							Memory: "3Gi",
						},
					},
					Args: []string{},
				},
			},
		},
	}
	if testSuite.Cluster != "" {
		prowConfig.Cluster = testSuite.Cluster
	} else if et.Job.Cluster != "" {
		prowConfig.Cluster = et.Job.Cluster
	}
	if !testSuite.Resources.isEmpty() {
		prowConfig.Spec.Containers[0].Resources = testSuite.Resources
	} else if !et.Job.Resources.isEmpty() {
		prowConfig.Spec.Containers[0].Resources = et.Job.Resources
	}
	// Possible weird assumtion
	if et.Job.Interval != "" {
		prowConfig.Cron = ""
		prowConfig.Interval = et.Job.Interval
	} else if et.Job.Cron != "" {
		prowConfig.Interval = ""
		prowConfig.Cron = et.Job.Cron
	} else {
		log.Fatalln("No interval or cron definition found")
	}
	// Assumes that the value in --timeout is of minutes.
	var timeout int
	var err error
	for _, arg := range testSuite.Args {
		if strings.HasPrefix(arg, "--timeout=") {
			value := arg[10 : len(arg)-1]
			timeout, err = strconv.Atoi(value)
			if err != nil {
				log.Fatalf("error, parsing timeout of job: %s, %s", et.JobName, err)
			}
			break
		}
	}
	newTimeout := fmt.Sprintf("%vm", timeout+20)
	prowConfig.DecorationConfig.Timeout = newTimeout
	return prowConfig
}

func newE2ENodeTest(jobName string, job Job, config ConfigFile) E2ENodeTest {
	fields := strings.Split(jobName, "-")
	if len(fields) != 6 {
		log.Fatalln("Expected 6 fields in job name", jobName)
	}
	return E2ENodeTest{
		JobName:    jobName,
		Job:        job,
		fields:     fields,
		Common:     config.Common,
		Image:      config.Images[fields[3]],
		K8SVersion: config.NodeK8SVersions[fields[4][3:]],
		TestSuite:  config.TestSuites[fields[5]],
	}
}

func (ent *E2ENodeTest) getJobDefinition(args []string) Job {
	rSigOwner := ent.Job.SigOwners
	if len(rSigOwner) == 0 {
		rSigOwner = []string{"UNKOWN"}
	}

	return Job{
		Scenario:  "kubernetes_e2e",
		Args:      args,
		SigOwners: rSigOwner,
	}
}

func (ent *E2ENodeTest) getProwConfig(testSuite TestSuite, k8sVersion NodeK8SVersion) Periodic {
	prowConfig := Periodic{
		Name: ent.JobName,
		Tags: []string{"generated"},
		Labels: map[string]string{
			"preset-service-account": "true",
			"preset-k8s-ssh":         "true",
		},
		Decorate: true,
		DecorationConfig: DecorationConfig{
			Timeout: "180m",
		},
		Spec: Spec{
			Containers: []Container{
				{
					Image: "gcr.io/k8s-staging-test-infra/kubekins-e2e:v20231206-f7b83ffbe6-master",
					Resources: Resources{
						Requests: ComputeResources{
							CPU:    "1000m",
							Memory: "3Gi",
						},
						Limits: ComputeResources{
							CPU:    "1000m",
							Memory: "3Gi",
						},
					},
					Args: []string{},
				},
			},
		},
	}
	if testSuite.Cluster != "" {
		prowConfig.Cluster = testSuite.Cluster
	} else if ent.Job.Cluster != "" {
		prowConfig.Cluster = ent.Job.Cluster
	}
	if !testSuite.Resources.isEmpty() {
		prowConfig.Spec.Containers[0].Resources = testSuite.Resources
	} else if !ent.Job.Resources.isEmpty() {
		prowConfig.Spec.Containers[0].Resources = ent.Job.Resources
	}
	// Possible weird assumtion
	if ent.Job.Interval != "" {
		prowConfig.Cron = ""
		prowConfig.Interval = ent.Job.Interval
	} else if ent.Job.Cron != "" {
		prowConfig.Interval = ""
		prowConfig.Cron = ent.Job.Cron
	} else {
		log.Fatalln("No interval or cron definition found")
	}
	// Assumes that the value in --timeout is of minutes.
	var timeout int
	var err error
	for _, arg := range testSuite.Args {
		if strings.HasPrefix(arg, "--timeout=") {
			value := arg[10 : len(arg)-1]
			timeout, err = strconv.Atoi(value)
			if err != nil {
				log.Fatalf("error, parsing timeout of job: %s, %s", ent.JobName, err)
			}
			break
		}
	}
	newTimeout := fmt.Sprintf("%vm", timeout+20)
	prowConfig.DecorationConfig.Timeout = newTimeout
	prowConfig.Spec.Containers[0].Args = append(prowConfig.Spec.Containers[0].Args, k8sVersion.Args...)
	prowConfig.Spec.Containers[0].Args = append(prowConfig.Spec.Containers[0].Args, "--root=/go/src")
	// Specify the appropriate kubekins-e2e image. This allows us to use a
	// specific image (containing a particular Go version) to build and
	// trigger the node e2e test to avoid issues like
	// https://github.com/kubernetes/kubernetes/issues/43534.
	if k8sVersion.ProwImage != "" {
		prowConfig.Spec.Containers[0].Image = k8sVersion.ProwImage
	}
	return prowConfig
}

func (ent *E2ENodeTest) generate() (Job, Periodic) {
	log.Printf("generating e2eNode job: %s", ent.JobName)
	if len(ent.fields) != 6 {
		log.Fatalln("Expected 6 fields in job name", ent.JobName)
	}
	image := ent.Image
	K8SVersion := ent.K8SVersion
	testSuite := ent.TestSuite
	// ENV check but in golang exclipt structs so not sure if needs to be allowed in structs
	args := []string{}
	args = append(args, getArgs(ent.JobName, ent.Common.Args)...)
	args = append(args, getArgs(ent.JobName, image.Args)...)
	// args = append(args, getArgs(ent.JobName, K8SVersion.Args)...)
	args = append(args, getArgs(ent.JobName, testSuite.Args)...)

	jobConfig := ent.getJobDefinition(args)
	prowConfig := ent.getProwConfig(testSuite, K8SVersion)

	nodeArgs := []string{}
	jobArgs := []string{}
	nodeFlag := "--node-args="
	for _, arg := range jobConfig.Args {
		if strings.Contains(arg, nodeFlag) {
			nodeArgs = append(nodeArgs, strings.SplitN(arg, "=", 2)[1])
		} else {
			jobArgs = append(jobArgs, arg)
		}
	}
	if len(nodeArgs) != 0 {
		for _, arg := range nodeArgs {
			nodeFlag += arg + " "
		}
		jobArgs = append(jobArgs, strings.TrimSpace(nodeFlag))
	}
	jobConfig.Args = jobArgs

	if prowConfig.Annotations == nil {
		prowConfig.Annotations = map[string]string{}
	}
	if image.TestgridPrefix != "" {
		dashboard := fmt.Sprintf("%s-%s-%s", image.TestgridPrefix, ent.fields[3], ent.fields[4])
		prowConfig.Annotations["testgrid-dashboards"] = dashboard
		tabName := fmt.Sprintf("%s-%s-%s", ent.fields[3], ent.fields[4], ent.fields[5])
		prowConfig.Annotations["testgrid-tab-name"] = tabName
	}
	return jobConfig, prowConfig
}

type ConfigFile struct {
	Jobs            map[string]Job            `yaml:"jobs"`
	CloudProviders  map[string]CloudProvider  `yaml:"cloudProviders"`
	Common          Common                    `yaml:"common"`
	Images          map[string]Image          `yaml:"images"`
	K8SVersions     map[string]K8SVersion     `yaml:"k8sVersions"`
	TestSuites      map[string]TestSuite      `yaml:"testSuites"`
	NodeTestSuites  map[string]TestSuite      `yaml:"nodeTestSuites"` // Only args
	NodeK8SVersions map[string]NodeK8SVersion `yaml:"nodeK8sVersions"`
	NodeImages      map[string]Image          `yaml:"nodeImages"`
	NodeCommon      Common                    `yaml:"nodeCommon"`
}

type E2ETest struct {
	EnvFilename   string
	JobName       string
	fields        []string
	Job           Job
	Common        Common
	CloudProvider CloudProvider
	Image         Image
	K8SVersion    K8SVersion
	TestSuite     TestSuite
}

type E2ENodeTest struct {
	JobName    string
	fields     []string
	Job        Job
	Common     Common
	Image      Image
	K8SVersion NodeK8SVersion
	TestSuite  TestSuite
}

type NodeK8SVersion struct {
	Args      []string `yaml:"args"`
	ProwImage string   `yaml:"prowImage"`
}

// Common/Shared
type Job struct {
	Scenario                   string // `yaml:"interval"`
	Interval                   string `yaml:"interval"`
	Cron                       string
	SigOwners                  []string `yaml:"sigOwners"`
	ReleaseBlocking            bool     `yaml:"releaseBlocking"`
	ReleaseInforming           bool     `yaml:"releaseInforming"`
	Cluster                    string   `yaml:"cluster"`
	TestgridNumFailuresToAlert int      `yaml:"testgridNumFailuresToAlert"`
	Args                       []string `yaml:"args"`
	Resources                  Resources
}

type Common struct {
	Args           []string
	TestgridPrefix string `yaml:"testgrid_prefix"`
}

type CloudProvider struct {
	Args []string
}

type Image struct {
	Args           []string
	TestgridPrefix string `yaml:"testgrid_prefix"`
}

type Resources struct {
	Requests ComputeResources `yaml:"requests"`
	Limits   ComputeResources `yaml:"limits"`
}

type ComputeResources struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

func (cr *ComputeResources) isEmpty() bool {
	return cr.CPU != "" || cr.Memory != ""
}

func (r *Resources) isEmpty() bool {
	return !r.Limits.isEmpty() || !r.Requests.isEmpty()
}

type K8SVersion struct {
	Args    []string `yaml:"args"`
	Version string   `yaml:"version"`
}

type TestSuite struct {
	Args      []string  `yaml:"args"`
	Resources Resources `yaml:"resources"`
	Cluster   string    `yaml:"cluster"`
}

// Prow Config Generated File
type ProwConfigFile struct {
	Periodics []Periodic `yaml:"periodics"`
}

type Periodic struct {
	Tags             []string          `yaml:"tags"`
	Interval         string            `yaml:"interval"`
	Cron             string            `yaml:"cron"`
	Labels           map[string]string `yaml:"labels"`
	Decorate         bool              `yaml:"decorate"`
	DecorationConfig DecorationConfig  `yaml:"decoration_config"`
	Name             string            `yaml:"name"`
	Spec             Spec              `yaml:"spec"`
	Cluster          string            `yaml:"cluster"`
	Annotations      map[string]string `yaml:"annotations"`
}

type DecorationConfig struct {
	Timeout string `yaml:"timeout"`
}

type Spec struct {
	Containers []Container `yaml:"containers"`
}

type Container struct {
	Command   []string  `yaml:"command"`
	Args      []string  `yaml:"args"`
	Env       string    `yaml:"env"`
	Image     string    `yaml:"image"`
	Resources Resources `yaml:"resources"`
}

// Testgrid
type TestgridConfig struct {
	TestGroups []TestGroup `json:"test_groups"`
}

type TestGroup struct {
	Name         string               `json:"name"`
	GCSPrefix    string               `json:"gcs_prefix"`
	ColumnHeader []ConfigurationValue `json:"column_header"`
}

func (tg *TestGroup) isEmpty() bool {
	return tg.Name != "" || tg.GCSPrefix != "" || len(tg.ColumnHeader) != 0
}

type ConfigurationValue struct {
	ConfigurationValue string `json:"configuration_value"`
}
