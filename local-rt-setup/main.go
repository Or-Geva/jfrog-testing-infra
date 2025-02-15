package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mholt/archiver/v3"
)

const maxConnectionWaitSeconds = 300
const waitSleepIntervalSeconds = 10
const jfrogHomeEnv = "JFROG_HOME"
const licenseEnv = "RTLIC"
const localArtifactoryUrl = "http://localhost:8081/artifactory/"
const defaultUsername = "admin"
const defaultPassword = "password"
const defaultVersion = "[RELEASE]"

func main() {
	err := setupLocalArtifactory()
	if err != nil {
		log.Fatal(err)
	}
}

func setupLocalArtifactory() (err error) {
	license := os.Getenv(licenseEnv)
	if license == "" {
		return errors.New("no license provided. Aborting. Provide license by setting the '" + licenseEnv + "' env var")
	}

	jfrogHome := os.Getenv(jfrogHomeEnv)
	if jfrogHome == "" {
		jfrogHome, err = setJfrogHome()
		if err != nil {
			return err
		}
	}

	exists, err := isExists(filepath.Join(jfrogHome, "artifactory"))
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("artifactory dir already exists in jfrog home: " + filepath.Join(jfrogHome, "artifactory"))
	}

	rtVersion := flag.String("rt-version", defaultVersion, "the version of Artifactory to download")
	flag.Parse()
	artifactory6 := false
	if *rtVersion != defaultVersion {
		versionParts := strings.Split(*rtVersion, ".")
		if len(versionParts) != 3 {
			return errors.New("the Artifactory version is invalid. It must be [RELEASE] or match this format: X.X.X")
		}
		majorVer, err := strconv.Atoi(versionParts[0])
		if err != nil {
			return err
		}
		if majorVer < 6 {
			return errors.New("this tool supports Artifactory 6 or higher")
		}
		artifactory6 = majorVer == 6
	}

	pathToArchive, err := downloadArtifactory(jfrogHome, *rtVersion, artifactory6)
	if err != nil {
		return err
	}

	err = extract(pathToArchive, jfrogHome)
	if err != nil {
		return err
	}

	err = renameArtifactoryDir(jfrogHome)
	if err != nil {
		return err
	}

	if !artifactory6 && isMac() {
		err = os.Chmod(filepath.Join(jfrogHome, "artifactory", "var"), os.ModePerm)
		if err != nil {
			return err
		}
	}

	err = createLicenseFile(jfrogHome, license, artifactory6)
	if err != nil {
		return err
	}

	var binDir string
	if artifactory6 {
		binDir = filepath.Join(jfrogHome, "artifactory", "bin")
	} else {
		binDir = filepath.Join(jfrogHome, "artifactory", "app", "bin")
	}
	err = startArtifactory(binDir)
	fmt.Println("error" + err.Error())
	if err != nil {
		return err
	}

	err = waitForArtifactorySuccessfulPing()
	if err != nil {
		return err
	}

	err = setCustomUrlBase()
	if err != nil || artifactory6 {
		return err
	}

	return enableArchiveIndex()
}

// Rename the directory that was extracted from the archive, to easily access in the rest of the script.
func renameArtifactoryDir(jfrogHome string) error {
	fileInfo, err := ioutil.ReadDir(jfrogHome)
	if err != nil {
		return err
	}

	for _, file := range fileInfo {
		if file.IsDir() && strings.HasPrefix(file.Name(), "artifactory-pro-") {
			return os.Rename(filepath.Join(jfrogHome, file.Name()), filepath.Join(jfrogHome, "artifactory"))
		}
	}
	return errors.New("artifactory dir was not found after extracting")
}

// Creates and sets the jfrog home directory at the parent of the working directory.
func setJfrogHome() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	jfrogHome := filepath.Join(filepath.Dir(wd), "jfrog_home")
	err = os.MkdirAll(jfrogHome, os.ModePerm)
	if err != nil {
		return "", err
	}

	err = os.Setenv(jfrogHomeEnv, jfrogHome)
	if err != nil {
		return "", err
	}
	return jfrogHome, err
}

func startArtifactory(binDir string) error {
	log.Println("Starting the Artifactory service...")
	var cmd *exec.Cmd
	if isWindows() {
		cmd = exec.Command(filepath.Join(binDir, "InstallService.bat"))
	} else {
		cmd = exec.Command(filepath.Join(binDir, "artifactoryctl"), "start")
	}
	// cmd.Stderr = os.Stderr
	// cmd.Stdout = os.Stderr
	fmt.Println("Run: " + strings.Join(cmd.Args, " "))
	return cmd.Run()
}

func waitForArtifactorySuccessfulPing() error {
	log.Println("Waiting for successful connection with Artifactory...")
	tryingLog := fmt.Sprintf("Trying again in %d seconds.", waitSleepIntervalSeconds)

	for timeElapsed := 0; timeElapsed < maxConnectionWaitSeconds; timeElapsed += waitSleepIntervalSeconds {
		time.Sleep(time.Second * waitSleepIntervalSeconds)

		response, err := ping()
		if err != nil {
			log.Printf("Receieved error: %s. %s", err, tryingLog)
		} else {
			err = response.Body.Close()
			if err != nil {
				return err
			}
			if response.StatusCode == http.StatusOK {
				log.Println("Artifactory is up!")
				return nil
			} else {
				log.Printf("Artifactory response: %d. %s", response.StatusCode, tryingLog)
			}
		}
	}
	return errors.New("could not connect to Artifactory")
}

func ping() (*http.Response, error) {
	url := localArtifactoryUrl + "api/system/ping"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	return http.DefaultClient.Do(req)
}

// Custom URL base is required when creating federated repositories.
func setCustomUrlBase() error {
	log.Println("Setting custom URL base...")

	url := localArtifactoryUrl + "api/system/configuration/baseUrl"
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer([]byte(localArtifactoryUrl)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}

	// Artifactory might return 500 because the url has allegedly changed.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("failed setting custom url. response: %d", resp.StatusCode)
	}

	// Verify connection after setting custom url.
	resp, err = ping()
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed reaching to Artifactory after setting custom url base. response: %d", resp.StatusCode)
	}

	log.Println("Done setting custom URL base.")
	return nil
}

func downloadArtifactory(downloadDest, rtVersion string, artifactory6 bool) (pathToArchive string, err error) {
	url := fmt.Sprintf("https://releases.jfrog.io/artifactory/artifactory-pro/org/artifactory/pro/jfrog-artifactory-pro/%[1]s/jfrog-artifactory-pro-%[1]s", rtVersion)
	if !artifactory6 {
		switch runtime.GOOS {
		case "darwin":
			url += "-darwin.tar.gz"
		case "windows":
			url += "-windows.zip"
		case "linux":
			url += "-linux.tar.gz"
		default:
			return "", errors.New("the OS on this machine is currently unsupported. Supported OS are darwin, windows and linux")
		}
	} else {
		url += ".zip"
	}

	log.Println("Downloading Artifactory from URL: " + url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed creating new request: %s", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed getting archive: %s", err)
	}
	defer func() {
		if e := resp.Body.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing body after download: " + e.Error())
			}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New("failed downloading Artifactory. Releases response: " + resp.Status)
	}

	// Extract archive file name.
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		return "", err
	}
	filename := params["filename"]
	log.Println("Extracted archive name from response: " + filename)

	pathToArchive = filepath.Join(downloadDest, filename)
	file, err := os.Create(pathToArchive)
	if err != nil {
		return "", err
	}
	defer func() {
		if e := file.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing archive file: " + e.Error())
			}
		}
	}()
	_, err = io.Copy(file, resp.Body)
	return pathToArchive, err
}

func extract(archivePath string, destDir string) error {
	log.Println("Extracting archive...")
	return archiver.Unarchive(archivePath, destDir)
}

func createLicenseFile(jfrogHome, license string, artifactory6 bool) (err error) {
	log.Println("Creating license...")

	defer func() {
		if e := os.Unsetenv(licenseEnv); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when unsetting env: " + e.Error())
			}
		}
	}()

	var fileDest string
	if artifactory6 {
		fileDest = filepath.Join(jfrogHome, "artifactory", "etc", "artifactory.lic")
	} else {
		fileDest = filepath.Join(jfrogHome, "artifactory", "var", "etc", "artifactory", "artifactory.cluster.license")
	}
	return os.WriteFile(fileDest, []byte(license), 0500)
}

func isExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func isMac() bool {
	return runtime.GOOS == "darwin"
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}

func enableArchiveIndex() error {
	log.Println("Enabling archive index...")
	confStr, err := handleConfiguration("GET", nil)
	if err != nil {
		return err
	}

	if !strings.Contains(confStr, getArchiveIndexEnabledAttribute(false)) {
		return errors.New("failed setting the archive index property - attribute does not exist in configuration")
	}
	confStr = strings.Replace(confStr, getArchiveIndexEnabledAttribute(false), getArchiveIndexEnabledAttribute(true), -1)

	// Post new configuration
	_, err = handleConfiguration("POST", strings.NewReader(confStr))
	return err
}

func handleConfiguration(method string, body io.Reader) (string, error) {
	url := localArtifactoryUrl + "api/system/configuration"

	log.Println(method + "ing Artifactory configuration...")
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(defaultUsername, defaultPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if e := resp.Body.Close(); e != nil {
			if err == nil {
				err = e
			} else {
				log.Println("error when closing body after download: " + e.Error())
			}
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed %sing Artifactory configuration. response: %d", method, resp.StatusCode)
	}

	buf := new(strings.Builder)
	n, err := io.Copy(buf, resp.Body)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("failed reading response body")
	}
	return buf.String(), nil
}

func getArchiveIndexEnabledAttribute(value bool) string {
	return fmt.Sprintf("<archiveIndexEnabled>%v</archiveIndexEnabled>", value)
}
