package gandi

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
	"github.com/kolo/xmlrpc"
)

type Driver struct {
	*drivers.BaseDriver
	MachineName    string
	ApiKey         string
	Url            string
	VmID           int
	Image          string
	IPAddress      string
	Datacenter     string
	storePath      string
	CaCertPath     string
	PrivateKeyPath string
	SSHUser        string
	SSHPort        int
}

const (
	dockerConfigDir   = "/etc/docker"
	defaultImage      = "Ubuntu 14.04 64 bits LTS (HVM)"
	defaultDatacenter = "Bissen"
	defaultUrl        = "https://rpc.gandi.net/xmlrpc/"
)

// GetCreateFlags registers the flags this driver adds to
// "docker hosts create"
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			EnvVar: "GANDI_APIKEY",
			Name:   "gandi-api-key",
			Usage:  "Gandi Api key",
		},
		mcnflag.StringFlag{
			EnvVar: "GANDI_IMAGE",
			Name:   "gandi-image",
			Usage:  "gandi Image",
			Value:  defaultImage,
		},
		mcnflag.StringFlag{
			EnvVar: "GANDI_DATACENTER",
			Name:   "gandi-dc",
			Usage:  "Gandi datacenter",
			Value:  defaultDatacenter,
		},
		mcnflag.StringFlag{
			EnvVar: "GANDI_URL",
			Name:   "gandi-url",
			Usage:  "Gandi Api url",
			Value:  defaultUrl,
		},
	}
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {

	d.ApiKey = flags.String("gandi-api-key")
	d.Image = flags.String("gandi-image")
	d.Datacenter = flags.String("gandi-dc")
	d.Url = flags.String("gandi-url")

	if d.ApiKey == "" {
		return fmt.Errorf("gandi driver requires the -gandi-api-key option")
	}

	return nil
}

func NewDriver(hostName, storePath string) *Driver {
	return &Driver{
		Image:      defaultImage,
		Datacenter: defaultDatacenter,
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}

func (d *Driver) DriverName() string {
	return "gandi"
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) PreCreateCheck() error {
	// TODO : check valid datacenter and ?
	return nil
}

// Helpers functions
func (d *Driver) vmById(id int) (VmInfo, error) {
	var res = VmInfo{}
	params := []interface{}{d.ApiKey, id}
	if err := d.getClient().Call("hosting.vm.info", params, &res); err != nil {
		return VmInfo{}, err
	}
	return res, nil
}

func (d *Driver) vmByName(name string) (VmInfo, error) {
	var res = []VmInfo{}
	var filter = map[string]string{"hostname": name}
	params := []interface{}{d.ApiKey, filter}
	if err := d.getClient().Call("hosting.vm.list", params, &res); err != nil {
		fmt.Printf("err : %v", err)
		return VmInfo{}, err
	}
	if len(res) != 1 {
		return VmInfo{}, errors.New("Vm not found")
	}
	return d.vmById(res[0].Id)
}

func (d *Driver) datacenterByName(name string) (DatacenterInfo,
	error) {
	var res = []DatacenterInfo{}
	var filter = map[string]string{"name": name}
	params := []interface{}{d.ApiKey, filter}
	if err := d.getClient().Call("hosting.datacenter.list", params, &res); err != nil {
		fmt.Printf("err : %v", err)
		return DatacenterInfo{}, err
	}
	if len(res) != 1 {
		return DatacenterInfo{}, errors.New("Datacenter not found")
	}
	return res[0], nil
}

func (d *Driver) imageByName(name string, zone_id int) (ImageInfo, error) {
	var res = []ImageInfo{}
	var filter = ImageFilter{Name: name, DcId: zone_id}
	params := []interface{}{d.ApiKey, filter}
	if err := d.getClient().Call("hosting.image.list", params, &res); err != nil {
		return ImageInfo{}, err
	}
	if len(res) != 1 {
		return ImageInfo{}, errors.New("Image not found")
	}
	return res[0], nil
}

func (d *Driver) waitForOp(op int) error {
	var res = OperationInfo{}
	params := []interface{}{d.ApiKey, op}
	if err := d.getClient().Call("operation.info", params, &res); err != nil {
		return err
	}
	for res.Status != "DONE" {
		log.Debugf("Waiting for operation #%d", op)
		time.Sleep(5 * time.Second)
		if err := d.getClient().Call("operation.info", params, &res); err != nil {
			log.Errorf("Got compute.Operation, err: %#v, %v", op, err)
			return err
		}
		if res.Status == "DONE" {
			return nil
		}
		if res.Status != "BILL" && res.Status != "WAIT" && res.Status != "RUN" {
			log.Errorf("Error waiting for operation: %d\n", op)
			return errors.New(fmt.Sprintf("Bad operation: %d", op))
		}
	}
	return nil
}

func (d *Driver) Create() error {
	sshKey, err := d.createSSHKey()
	if err != nil {
		return err
	}

	log.Infof("Creating Gandi server...")
	dc, err := d.datacenterByName(d.Datacenter)
	if err != nil {
		return err
	}

	image, err := d.imageByName(d.Image, dc.Id)
	if err != nil {
		return err
	}
	vmReq := VmCreateRequest{
		DcId:       dc.Id,
		Hostname:   d.MachineName,
		Memory:     512,
		Cores:      1,
		IpVersion:  4,
		SshKey:     sshKey,
		RunCommand: "apt-get install -y sudo && curl -sSL https://get.docker.com/ | sh",
	}
	diskReq := DiskCreateRequest{
		Name: d.MachineName,
		DcId: dc.Id,
		Size: 5120,
	}
	var res = []OperationInfo{}
	params := []interface{}{d.ApiKey, vmReq, diskReq, image.DiskId}
	if err := d.getClient().Call("hosting.vm.create_from", params, &res); err != nil {
		return err
	}
	if err := d.waitForOp(res[2].Id); err != nil {
		return err
	}
	vm, err := d.vmByName(d.MachineName)
	if err != nil {
		return err
	}

	d.VmID = vm.Id
	d.IPAddress = vm.NetworkInterfaces[0].Ips[0].Ip

	return nil
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

func (d *Driver) GetState() (state.State, error) {
	params := []interface{}{d.ApiKey, d.VmID}
	res := VmInfo{}
	err := d.getClient().Call("hosting.vm.info", params, &res)
	if err != nil {
		return state.Error, err
	}
	switch res.State {
	case "being_created":
		return state.Starting, nil
	case "paused", "locked", "legally_locked":
		return state.Paused, nil
	case "running":
		return state.Running, nil
	case "halted":
		return state.Stopped, nil
	case "deleted":
		return state.Stopped, nil
	case "invalid":
		return state.Error, nil
	}
	return state.None, nil
}

func (d *Driver) Start() error {
	params := []interface{}{d.ApiKey, d.VmID}
	res := OperationInfo{}
	err := d.getClient().Call("hosting.vm.start", params, &res)
	if err != nil {
		return err
	}
	if err := d.waitForOp(res.Id); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Stop() error {
	params := []interface{}{d.ApiKey, d.VmID}
	res := OperationInfo{}
	err := d.getClient().Call("hosting.vm.stop", params, &res)
	if err != nil {
		return err
	}
	if err := d.waitForOp(res.Id); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Remove() error {
	vm_state, err := d.GetState()
	if vm_state == state.Running {
		err := d.Stop()
		if err != nil {
			return err
		}
	}
	params := []interface{}{d.ApiKey, d.VmID}
	res := OperationInfo{}
	err = d.getClient().Call("hosting.vm.delete", params, &res)
	if err != nil {
		return err
	}
	if err := d.waitForOp(res.Id); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Restart() error {
	params := []interface{}{d.ApiKey, d.VmID}
	res := OperationInfo{}
	err := d.getClient().Call("hosting.vm.reboot", params, &res)
	if err != nil {
		return err
	}
	if err := d.waitForOp(res.Id); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Kill() error {
	return d.Stop()
}

func (d *Driver) getClient() *xmlrpc.Client {
	rpc, err := xmlrpc.NewClient(d.Url, nil)
	if err != nil {
		return nil
	}
	return rpc
}

func (d *Driver) createSSHKey() (string, error) {
	if err := ssh.GenerateSSHKey(d.sshKeyPath()); err != nil {
		return "", err
	}

	publicKey, err := ioutil.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return "", err
	}

	return string(publicKey), nil
}

func (d *Driver) sshKeyPath() string {
	return filepath.Join(d.storePath, "id_rsa")
}

func (d *Driver) publicSSHKeyPath() string {
	return d.sshKeyPath() + ".pub"
}
