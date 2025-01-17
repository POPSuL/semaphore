package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/ansible-semaphore/semaphore/api/helpers"
	"github.com/ansible-semaphore/semaphore/api/sockets"
	"github.com/ansible-semaphore/semaphore/db"
	"github.com/ansible-semaphore/semaphore/util"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	taskRunningStatus  = "running"
	taskWaitingStatus  = "waiting"
	taskStoppingStatus = "stopping"
	taskStoppedStatus  = "stopped"
	taskSuccessStatus  = "success"
	taskFailStatus     = "error"
	taskTypeID         = "task"
)

type task struct {
	store       db.Store
	task        db.Task
	template    db.Template
	inventory   db.Inventory
	repository  db.Repository
	environment db.Environment
	users       []int
	projectID   int
	hosts       []string
	alertChat   string
	alert       bool
	prepared    bool
	process     *os.Process
}

func (t *task) getRepoName() string {
	return "repository_" + strconv.Itoa(t.repository.ID) + "_" + strconv.Itoa(t.template.ID)
}

func (t *task) getRepoPath() string {
	return util.Config.TmpPath + "/" + t.getRepoName()
}

func (t *task) setStatus(status string) {
	if t.task.Status == taskStoppingStatus {
		switch status {
		case taskFailStatus:
			status = taskStoppedStatus
		case taskStoppedStatus:
		default:
			panic("stopping task cannot be " + status)
		}
	}
	t.task.Status = status
	t.updateStatus()
}

func (t *task) updateStatus() {
	for _, user := range t.users {
		b, err := json.Marshal(&map[string]interface{}{
			"type":       "update",
			"start":      t.task.Start,
			"end":        t.task.End,
			"status":     t.task.Status,
			"task_id":    t.task.ID,
			"project_id": t.projectID,
		})

		util.LogPanic(err)

		sockets.Message(user, b)
	}

	if err := t.store.UpdateTask(t.task); err != nil {
		t.panicOnError(err, "Failed to update task status")
	}
}

func (t *task) fail() {
	t.setStatus(taskFailStatus)
	t.sendMailAlert()
	t.sendTelegramAlert()
}

func (t *task) destroyKeys() {
	err := t.destroyKey(t.repository.SSHKey)
	if err != nil {
		t.log("Can't destroy repository SSH key, error: " + err.Error())
	}
	err = t.destroyKey(t.inventory.SSHKey)
	if err != nil {
		t.log("Can't destroy inventory SSH key, error: " + err.Error())
	}
}

func (t *task) createTaskEvent() {
	objType := taskTypeID
	desc := "Task ID " + strconv.Itoa(t.task.ID) + " (" + t.template.Alias + ")" + " finished - " + strings.ToUpper(t.task.Status)

	_, err := t.store.CreateEvent(db.Event{
		UserID:      t.task.UserID,
		ProjectID:   &t.projectID,
		ObjectType:  &objType,
		ObjectID:    &t.task.ID,
		Description: &desc,
	})

	if err != nil {
		t.panicOnError(err, "Fatal error inserting an event")
	}
}

func (t *task) prepareRun() {
	t.prepared = false

	defer func() {
		log.Info("Stopped preparing task " + strconv.Itoa(t.task.ID))
		log.Info("Release resource locker with task " + strconv.Itoa(t.task.ID))
		resourceLocker <- &resourceLock{lock: false, holder: t}

		t.createTaskEvent()
	}()

	t.log("Preparing: " + strconv.Itoa(t.task.ID))

	err := checkTmpDir(util.Config.TmpPath)
	if err != nil {
		t.log("Creating tmp dir failed: " + err.Error())
		t.fail()
		return
	}

	if err := t.populateDetails(); err != nil {
		t.log("Error: " + err.Error())
		t.fail()
		return
	}

	objType := taskTypeID
	desc := "Task ID " + strconv.Itoa(t.task.ID) + " (" + t.template.Alias + ")" + " is preparing"
	_, err = t.store.CreateEvent(db.Event{
		UserID:      t.task.UserID,
		ProjectID:   &t.projectID,
		ObjectType:  &objType,
		ObjectID:    &t.task.ID,
		Description: &desc,
	})

	if err != nil {
		t.log("Fatal error inserting an event")
		panic(err)
	}

	t.log("Prepare task with template: " + t.template.Alias + "\n")

	if err := t.installKey(t.repository.SSHKey); err != nil {
		t.log("Failed installing ssh key for repository access: " + err.Error())
		t.fail()
		return
	}

	if err := t.updateRepository(); err != nil {
		t.log("Failed updating repository: " + err.Error())
		t.fail()
		return
	}

	if err := t.installInventory(); err != nil {
		t.log("Failed to install inventory: " + err.Error())
		t.fail()
		return
	}

	if err := t.installRequirements(); err != nil {
		t.log("Running galaxy failed: " + err.Error())
		t.fail()
		return
	}

	if err := t.installVaultPassFile(); err != nil {
		t.log("Failed to install vault password file: " + err.Error())
		t.fail()
		return
	}

	// todo: write environment

	if stderr, err := t.listPlaybookHosts(); err != nil {
		t.log("Listing playbook hosts failed: " + err.Error() + "\n" + stderr)
		t.fail()
		return
	}

	t.prepared = true
}

func (t *task) run() {
	defer func() {
		log.Info("Stopped running task " + strconv.Itoa(t.task.ID))
		log.Info("Release resource locker with task " + strconv.Itoa(t.task.ID))
		resourceLocker <- &resourceLock{lock: false, holder: t}

		now := time.Now()
		t.task.End = &now
		t.updateStatus()
		t.createTaskEvent()
		t.destroyKeys()
	}()

	if t.task.Status == taskStoppingStatus {
		t.setStatus(taskStoppedStatus)
		return
	}

	{
		now := time.Now()
		t.task.Start = &now
		t.setStatus(taskRunningStatus)
	}

	objType := taskTypeID
	desc := "Task ID " + strconv.Itoa(t.task.ID) + " (" + t.template.Alias + ")" + " is running"

	_, err := t.store.CreateEvent(db.Event{
		UserID:      t.task.UserID,
		ProjectID:   &t.projectID,
		ObjectType:  &objType,
		ObjectID:    &t.task.ID,
		Description: &desc,
	})

	if err != nil {
		t.log("Fatal error inserting an event")
		panic(err)
	}

	t.log("Started: " + strconv.Itoa(t.task.ID))
	t.log("Run task with template: " + t.template.Alias + "\n")

	if t.task.Status == taskStoppingStatus {
		t.setStatus(taskStoppedStatus)
		return
	}

	if err := t.runPlaybook(); err != nil {
		t.log("Running playbook failed: " + err.Error())
		t.fail()
		return
	}

	t.setStatus(taskSuccessStatus)
}

func (t *task) prepareError(err error, errMsg string) error {
	if err == db.ErrNotFound {
		t.log(errMsg)
		return err
	}

	if err != nil {
		t.fail()
		panic(err)
	}

	return nil
}

//nolint: gocyclo
func (t *task) populateDetails() error {
	// get template
	var err error

	t.template, err = t.store.GetTemplate(t.projectID, t.task.TemplateID)
	if err != nil {
		return t.prepareError(err, "Template not found!")
	}

	// get project alert setting
	project, err := t.store.GetProject(t.template.ProjectID)
	if err != nil {
		return t.prepareError(err, "Project not found!")
	}

	t.alert = project.Alert
	t.alertChat = project.AlertChat

	// get project users
	users, err := t.store.GetProjectUsers(t.template.ProjectID, db.RetrieveQueryParams{})
	if err != nil {
		return t.prepareError(err, "Users not found!")
	}

	t.users = []int{}
	for _, user := range users {
		t.users = append(t.users, user.ID)
	}

	// get inventory
	t.inventory, err = t.store.GetInventory(t.template.ProjectID, t.template.InventoryID)
	if err != nil {
		return t.prepareError(err, "Template Inventory not found!")
	}

	// get repository
	t.repository, err = t.store.GetRepository(t.template.ProjectID, t.template.RepositoryID)

	if err != nil {
		return err
	}

	//if t.repository.SSHKey.Type != db.AccessKeySSH {
	//	t.log("Repository Access Key is not 'SSH': " + t.repository.SSHKey.Type)
	//	return errors.New("unsupported SSH Key")
	//}

	// get environment
	if len(t.task.Environment) == 0 && t.template.EnvironmentID != nil {
		t.environment, err = t.store.GetEnvironment(t.template.ProjectID, *t.template.EnvironmentID)
		if err != nil {
			return err
		}
	} else if len(t.task.Environment) > 0 {
		t.environment.JSON = t.task.Environment
	}

	return nil
}

func (t *task) destroyKey(key db.AccessKey) error {
	path := key.GetPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(path)
}

func (t *task) installVaultPassFile() error {
	if t.template.VaultPassID == nil {
		return nil
	}

	path := t.template.VaultPass.GetPath()

	return ioutil.WriteFile(path, []byte(t.template.VaultPass.LoginPassword.Password), 0600)
}

func (t *task) installKey(key db.AccessKey) error {
	if key.Type != db.AccessKeySSH {
		return nil
	}

	t.log("access key " + key.Name + " installed")

	path := key.GetPath()

	if key.SshKey.Passphrase != "" {
		return fmt.Errorf("ssh key with passphrase not supported")
	}

	return ioutil.WriteFile(path, []byte(key.SshKey.PrivateKey), 0600)
}

func (t *task) updateRepository() error {
	t.getRepoPath()
	repoName := t.getRepoName()
	_, err := os.Stat(t.getRepoPath())

	cmd := exec.Command("git") //nolint: gas
	cmd.Dir = util.Config.TmpPath

	switch t.repository.SSHKey.Type {
	case db.AccessKeySSH:
		gitSSHCommand := "ssh -o StrictHostKeyChecking=no -i " + t.repository.SSHKey.GetPath()
		cmd.Env = t.envVars(util.Config.TmpPath, util.Config.TmpPath, &gitSSHCommand)
	case db.AccessKeyNone:
		cmd.Env = t.envVars(util.Config.TmpPath, util.Config.TmpPath, nil)
	default:
		return fmt.Errorf("unsupported access key type: " + t.repository.SSHKey.Type)
	}

	repoURL, repoTag := t.repository.GitURL, "master"
	if split := strings.Split(repoURL, "#"); len(split) > 1 {
		repoURL, repoTag = split[0], split[1]
	}

	if err != nil && os.IsNotExist(err) {
		t.log("Cloning repository " + repoURL)
		cmd.Args = append(cmd.Args, "clone", "--recursive", "--branch", repoTag, repoURL, repoName)
	} else if err != nil {
		return err
	} else {
		t.log("Updating repository " + repoURL)
		cmd.Dir += "/" + repoName
		cmd.Args = append(cmd.Args, "pull", "origin", repoTag)
	}

	t.logCmd(cmd)
	return cmd.Run()
}

func (t *task) installRequirements() error {
	requirementsFilePath := fmt.Sprintf("%s/roles/requirements.yml", t.getRepoPath())
	requirementsHashFilePath := fmt.Sprintf("%s/requirements.md5", t.getRepoPath())

	if _, err := os.Stat(requirementsFilePath); err != nil {
		t.log("No roles/requirements.yml file found. Skip galaxy install process.\n")
		return nil
	}

	if hasRequirementsChanges(requirementsFilePath, requirementsHashFilePath) {
		if err := t.runGalaxy([]string{
			"install",
			"-r",
			"roles/requirements.yml",
			"--force",
		}); err != nil {
			return err
		}
		if err := writeMD5Hash(requirementsFilePath, requirementsHashFilePath); err != nil {
			return err
		}
	} else {
		t.log("roles/requirements.yml has no changes. Skip galaxy install process.\n")
	}

	return nil
}

func (t *task) runGalaxy(args []string) error {
	cmd := exec.Command("ansible-galaxy", args...) //nolint: gas
	cmd.Dir = t.getRepoPath()

	gitSSHCommand := "ssh -o StrictHostKeyChecking=no -i " + t.repository.SSHKey.GetPath()
	cmd.Env = t.envVars(util.Config.TmpPath, cmd.Dir, &gitSSHCommand)

	t.logCmd(cmd)
	return cmd.Run()
}

func (t *task) listPlaybookHosts() (string, error) {

	if util.Config.ConcurrencyMode == "project" {
		return "", nil
	}

	args, err := t.getPlaybookArgs()
	if err != nil {
		return "", err
	}
	args = append(args, "--list-hosts")

	cmd := exec.Command("ansible-playbook", args...) //nolint: gas
	cmd.Dir = t.getRepoPath()
	cmd.Env = t.envVars(util.Config.TmpPath, cmd.Dir, nil)

	var errb bytes.Buffer
	cmd.Stderr = &errb

	out, err := cmd.Output()

	re := regexp.MustCompile(`(?m)^\\s{6}(.*)$`)
	matches := re.FindAllSubmatch(out, 20)
	hosts := make([]string, len(matches))
	for i := range matches {
		hosts[i] = string(matches[i][1])
	}
	t.hosts = hosts
	return errb.String(), err
}

func (t *task) runPlaybook() (err error) {
	args, err := t.getPlaybookArgs()
	if err != nil {
		return
	}
	cmd := exec.Command("ansible-playbook", args...) //nolint: gas
	cmd.Dir = t.getRepoPath()
	cmd.Env = t.envVars(util.Config.TmpPath, cmd.Dir, nil)

	t.logCmd(cmd)
	cmd.Stdin = strings.NewReader("")
	err = cmd.Start()
	if err != nil {
		return
	}
	t.process = cmd.Process
	err = cmd.Wait()
	return
}

func (t *task) getExtraVars() (string, error) {
	extraVars := make(map[string]interface{})

	if t.inventory.SSHKey.Type == db.AccessKeyLoginPassword {
		if t.inventory.SSHKey.LoginPassword.Login != "" {
			extraVars["ansible_user"] = t.inventory.SSHKey.LoginPassword.Login
		}
		extraVars["ansible_password"] = t.inventory.SSHKey.LoginPassword.Password
	}

	if t.inventory.BecomeKey.Type == db.AccessKeyLoginPassword {
		if t.inventory.SSHKey.LoginPassword.Login != "" {
			extraVars["ansible_become_user"] = t.inventory.SSHKey.LoginPassword.Login
		}
		extraVars["ansible_become_password"] = t.inventory.SSHKey.LoginPassword.Password
	}

	if t.environment.JSON != "" {
		err := json.Unmarshal([]byte(t.environment.JSON), &extraVars)
		if err != nil {
			return "", err
		}
	}

	delete(extraVars, "ENV")

	ev, err := json.Marshal(extraVars)
	if err != nil {
		return "", err
	}

	return string(ev), nil
}

//nolint: gocyclo
func (t *task) getPlaybookArgs() ([]string, error) {
	playbookName := t.task.Playbook
	if len(playbookName) == 0 {
		playbookName = t.template.Playbook
	}

	var inventory string
	switch t.inventory.Type {
	case "file":
		inventory = t.inventory.Inventory
	default:
		inventory = util.Config.TmpPath + "/inventory_" + strconv.Itoa(t.task.ID)
	}

	args := []string{
		"-i", inventory,
	}

	if t.inventory.SSHKeyID != nil && t.inventory.SSHKey.Type == db.AccessKeySSH {
		args = append(args, "--private-key="+t.inventory.SSHKey.GetPath())
	}

	if t.task.Debug {
		args = append(args, "-vvvv")
	}

	if t.task.DryRun {
		args = append(args, "--check")
	}

	if t.template.VaultPassID != nil {
		args = append(args, "--vault-password-file", t.template.VaultPass.GetPath())
	}

	extraVars, err := t.getExtraVars()
	if err != nil {
		t.log(err.Error())
		t.log("Could not remove command environment, if existant it will be passed to --extra-vars. This is not fatal but be aware of side effects")
	} else if extraVars != "" {
		args = append(args, "--extra-vars", extraVars)
	}

	var templateExtraArgs []string
	if t.template.Arguments != nil {
		err := json.Unmarshal([]byte(*t.template.Arguments), &templateExtraArgs)
		if err != nil {
			t.log("Could not unmarshal arguments to []string")
			return nil, err
		}
	}

	var taskExtraArgs []string
	if t.task.Arguments != nil {
		err := json.Unmarshal([]byte(*t.task.Arguments), &taskExtraArgs)
		if err != nil {
			t.log("Could not unmarshal arguments to []string")
			return nil, err
		}
	}

	if t.template.OverrideArguments {
		args = templateExtraArgs
	} else {
		args = append(args, templateExtraArgs...)
		args = append(args, taskExtraArgs...)
		args = append(args, playbookName)
	}
	return args, nil
}

func (t *task) envVars(home string, pwd string, gitSSHCommand *string) []string {
	env := os.Environ()
	env = append(env, fmt.Sprintf("HOME=%s", home))
	env = append(env, fmt.Sprintf("PWD=%s", pwd))
	env = append(env, fmt.Sprintln("PYTHONUNBUFFERED=1"))
	//env = append(env, fmt.Sprintln("GIT_FLUSH=1"))
	env = append(env, extractCommandEnvironment(t.environment.JSON)...)

	if gitSSHCommand != nil {
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=%s", *gitSSHCommand))
	}

	return env
}

func hasRequirementsChanges(requirementsFilePath string, requirementsHashFilePath string) bool {
	oldFileMD5HashBytes, err := ioutil.ReadFile(requirementsHashFilePath)
	if err != nil {
		return true
	}

	newFileMD5Hash, err := helpers.GetMD5Hash(requirementsFilePath)
	if err != nil {
		return true
	}

	return string(oldFileMD5HashBytes) != newFileMD5Hash
}

func writeMD5Hash(requirementsFile string, requirementsHashFile string) error {
	newFileMD5Hash, err := helpers.GetMD5Hash(requirementsFile)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(requirementsHashFile, []byte(newFileMD5Hash), 0644)
}

// extractCommandEnvironment unmarshalls a json string, extracts the ENV key from it and returns it as
// []string where strings are in key=value format
func extractCommandEnvironment(envJSON string) []string {
	env := make([]string, 0)
	var js map[string]interface{}
	err := json.Unmarshal([]byte(envJSON), &js)
	if err == nil {
		if cfg, ok := js["ENV"]; ok {
			switch v := cfg.(type) {
			case map[string]interface{}:
				for key, val := range v {
					env = append(env, fmt.Sprintf("%s=%s", key, val))
				}
			}
		}
	}
	return env
}

// checkTmpDir checks to see if the temporary directory exists
// and if it does not attempts to create it
func checkTmpDir(path string) error {
	var err error
	if _, err = os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(path, 0700)
		}
	}
	return err
}
