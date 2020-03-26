package sls

import (
	"bytes"
	"errors"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	YamlName   = "serverless.yml"
	slsRetries = 10
)

type FunctionMeta struct {
	Name        string `yaml:"name"`
	Handler     string `yaml:"handler"`
	Description string `yaml:"description"`
	Runtime     string `yaml:"runtime"`
	MemorySize  string `yaml:"memorySize"`
}

type Functions map[string]FunctionMeta

type ServiceStack struct {
	StackId  string `yaml:"service"`
	Provider struct {
		Name    string `yaml:"name"`
		Project string `yaml:"project"`
		Stage   string `yaml:"stage"`
	}

	Functions Functions
}

type Wrapper struct {
	provider    string
	slsPath     string
	yamlDirPath string
	stack       *ServiceStack
	suffix      string
	Opts        map[string]string
}

func New(provider string, yamlDirPath string) (*Wrapper, error) {
	path, err := getSLSPath()
	if err != nil {
		return nil, errors.New("serverless framework is not installed")
	}

	stack, err := ParseConfig(provider, yamlDirPath)
	if err != nil {
		return nil, err
	}

	return &Wrapper{provider: provider, slsPath: path, yamlDirPath: yamlDirPath, stack: stack, Opts: make(map[string]string)}, nil
}

func getSLSPath() (string, error) {
	return exec.LookPath("sls")
}

func ParseConfig(provider string, yamlDirPath string) (*ServiceStack, error) {
	yamlData, err := ioutil.ReadFile(filepath.Join(yamlDirPath, YamlName))
	if err != nil {
		return nil, err
	}

	slsData := ServiceStack{}

	err = yaml.Unmarshal(yamlData, &slsData)
	if err != nil {
		return nil, err
	}
	if provider != slsData.Provider.Name {
		return nil, errors.New(fmt.Sprintf("expected provider %s, found provider: %s", provider, slsData.Provider.Name))
	}

	return &slsData, nil
}

func (w *Wrapper) ListFunctionsFromYaml() Functions {
	return w.stack.Functions
}

func (w *Wrapper) StackId() string {
	return strings.Replace(w.stack.StackId, "-${opt:suffix}", "", -1)
}

func (w *Wrapper) Project() string {
	return w.stack.Provider.Project
}

func (w *Wrapper) Stage() string {
	return w.stack.Provider.Stage
}

func (w *Wrapper) execCmd(env []string, dir string, command string, cmdArgs ...string) (string, error) {
	var stdoutBuf, stderrBuf bytes.Buffer
	var errStdout, errStderr error

	cwd := dir

	cmd := exec.Command(command, cmdArgs...)
	cmd.Dir = cwd

	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()

	stdout := io.MultiWriter(os.Stdout, &stdoutBuf)
	stderr := io.MultiWriter(os.Stderr, &stderrBuf)
	err := cmd.Start()
	if err != nil {
		return "", err
	}

	go func() {
		_, errStdout = io.Copy(stdout, stdoutIn)
	}()

	go func() {
		_, errStderr = io.Copy(stderr, stderrIn)
	}()

	err = cmd.Wait()
	if errStdout != nil || errStderr != nil {
		return "", errors.New("failed to capture stdout or stderr")
	}
	return strings.TrimSpace(stdoutBuf.String()), err
}

func (w *Wrapper) execSlsCmd(funcDir string, slsCmd ...string) (string, error) {
	slsCmd = append(slsCmd, "--suffix")
	slsCmd = append(slsCmd, w.suffix)

	for opt, optVal := range w.Opts {
		slsCmd = append(slsCmd, "--"+opt)
		slsCmd = append(slsCmd, optVal)
	}

	retries := slsRetries
	resp, err := w.execCmd([]string{}, funcDir, "sls", slsCmd...)
	for err != nil && retries > 0 {
		resp, err = w.execCmd([]string{}, funcDir, "sls", slsCmd...)
		time.Sleep(5 * time.Second)
		retries--
	}
	return resp, err
}

func (w *Wrapper) DeployStack() error {

	w.suffix = strconv.FormatInt(time.Now().UnixNano(), 10)

	functions := make(map[string]FunctionMeta)
	for k, v := range w.stack.Functions {
		v.Name = strings.Replace(v.Name, "${opt:suffix}", w.suffix, -1)
		functions[k] = v
	}
	w.stack.Functions = functions
	
	err := w.buildJava("java8")
	if err != nil {
		return err
	}
	err = w.buildJava("java11")
	if err != nil {
		return err
	}
	err = w.buildCsharp()
	if err != nil {
		return err
	}
	err = w.buildGolang()
	if err != nil {
		return err
	}
	_, err = w.execSlsCmd(w.yamlDirPath, "deploy", "--no-aws-s3-accelerate")
	return err
}

func (w *Wrapper) buildJava(version string) error {
	javaPath, javaInStack, err := w.platformPath(version)
	if err != nil {
		return err
	}
	if !javaInStack {
		return nil
	}
	_, err = w.execCmd([]string{}, javaPath, "mvn", "package")
	if err != nil && strings.HasPrefix(err.Error(), "WARNING") {
		return nil
	}
	return err
}

func (w *Wrapper) RemoveStack() error {
	_, err := w.execSlsCmd(w.yamlDirPath, "remove")
	return err
}

func (w *Wrapper) ListFunction() error {
	_, err := w.execSlsCmd(w.yamlDirPath, "deploy", "list", "functions")

	return err
}

func (w *Wrapper) buildCsharp() error {
	csharpPath, csharpInStack, err := w.platformPath("csharp")
	if err != nil {
		return err
	}
	if !csharpInStack {
		return nil
	}
	_, err = w.execCmd([]string{}, csharpPath, "dotnet", "restore")
	if err != nil {
		return err
	}
	_, err = w.execCmd([]string{},
		csharpPath,
		"dotnet",
		"lambda",
		"package",
		"--configuration",
		"release",
		"--framework",
		"netcoreapp2.1",
		"--output-package",
		"./deploy.zip")
	return err
}

func (w *Wrapper) platformPath(platform string) (string, bool, error) {
	srcPath := path.Join(w.yamlDirPath, platform)
	_, err := os.Stat(srcPath)
	if err != nil && os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return srcPath, true, nil
}

func (w *Wrapper) buildGolang() error {
	golangPath, goInStack, err := w.platformPath("golang")
	if err != nil {
		return err
	}
	if !goInStack {
		return nil
	}
	env := []string{"GOOS=linux", "GO111MODULE=on"}
	_, err = w.execCmd(env, golangPath, "go", "build", "-ldflags", "-s", "-ldflags", "-w", "-o", "bin/hello", "main.go")
	return err
}
