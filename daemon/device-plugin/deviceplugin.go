package nfdeviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	pb "github.com/openshift/dpu-operator/dpu-api/gen"
	"github.com/openshift/dpu-operator/dpu-cni/pkgs/cnitypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	VendorPluginSocketPath string = cnitypes.DaemonBaseDir + "vendor-plugin/vendor-plugin.sock"

	// Device plugin settings.
	pluginMountPath = "/var/lib/kubelet/device-plugins"
	kubeletEndpoint = "kubelet.sock"
	pluginEndpoint  = "sriovNet.sock"
	resourceName    = "openshift.io/dpu"
)

// sriovManager manages sriov networking devices
type nfResources struct {
	socketFile string
	devices    map[string]pluginapi.Device // for Kubelet DP API
	grpcServer *grpc.Server
	pluginapi.DevicePluginServer
	log    logr.Logger
	client pb.DeviceServiceClient
	conn   *grpc.ClientConn
}

type DevicePlugin interface {
	Start() error
}

func (nf *nfResources) sendDevices(stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	resp := new(pluginapi.ListAndWatchResponse)
	for _, dev := range nf.devices {
		resp.Devices = append(resp.Devices, &pluginapi.Device{ID: dev.ID, Health: dev.Health})
	}

	nf.log.Info("SendDevices:", "resp", resp)
	if err := stream.Send(resp); err != nil {
		nf.log.Error(err, "Cannot send devices to ListAndWatch server")
		nf.grpcServer.Stop()
		return err
	}
	return nil
}

func (nf *nfResources) getDeviceState(DeviceName string) string {
	// TODO: Discover device health
	return pluginapi.Healthy
}

func (nf *nfResources) changed() bool {
	changed := false
	for id, dev := range nf.devices {
		state := nf.getDeviceState(id)
		if dev.Health != state {
			changed = true
			dev.Health = state
			nf.devices[id] = dev
		}
	}
	return changed
}

func (nf *nfResources) ListAndWatch(empty *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	changed := true
	for {
		if changed {
			err := nf.sendDevices(stream)
			if err != nil {
				return err
			}
		}
		time.Sleep(5 * time.Second)
		changed = nf.changed()
	}
}

// Allocate passes the dev name as an env variable to the requesting container
func (nf *nfResources) Allocate(ctx context.Context, rqt *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := new(pluginapi.AllocateResponse)
	devName := ""
	for _, container := range rqt.ContainerRequests {
		containerResp := new(pluginapi.ContainerAllocateResponse)
		for _, id := range container.DevicesIDs {
			nf.log.Info("DeviceID in Allocate:", "id", id)
			dev, ok := nf.devices[id]
			if !ok {
				return nil, fmt.Errorf("invalid allocation request with non-existing device: %s", id)
			}
			if dev.Health != pluginapi.Healthy {
				return nil, fmt.Errorf("invalid allocation request with unhealthy device: %s", id)
			}

			devName = devName + id + ","
		}

		nf.log.Info("Device(s) allocated:", "devName", devName)
		envmap := make(map[string]string)
		envmap["NF-DEV"] = devName

		containerResp.Envs = envmap
		resp.ContainerResponses = append(resp.ContainerResponses, containerResp)
	}
	return resp, nil
}

func (nf *nfResources) GetDevices() error {
	err := nf.ensureConnected()
	if err != nil {
		return fmt.Errorf("failed to ensure connection to plugin: %v", err)
	}

	Devices, err := nf.client.GetDevices(context.Background(), &pb.Empty{})
	if err != nil {
		return fmt.Errorf("failed to handle GetDevices request: %v", err)
	}

	for _, device := range Devices.Devices {
		nf.devices[device.ID] = pluginapi.Device{ID: device.ID, Health: pluginapi.Healthy}
		nf.log.Info("Found device", "device.ID", device.ID)
	}

	return nil
}

func (nf *nfResources) RegisterDevicePlugin() error {
	pluginEndpoint := filepath.Join(pluginapi.DevicePluginPath, nf.socketFile)
	nf.log.Info("Starting Device Plugin server at:", "pluginEndpoint", pluginEndpoint)
	lis, err := net.Listen("unix", pluginEndpoint)
	if err != nil {
		return fmt.Errorf("resource %s failed to listen to Device Plugin server: %v", resourceName, err)
	}
	nf.grpcServer = grpc.NewServer()

	kubeletEndpoint := filepath.Join("unix:", DeprecatedSockDir, KubeEndPoint)
	conn, err := grpc.Dial(kubeletEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("resource %s unable connect to Kubelet: %v", resourceName, err)
	}
	defer conn.Close()

	pluginapi.RegisterDevicePluginServer(nf.grpcServer, nf)

	client := pluginapi.NewRegistrationClient(conn)

	go func() {
		err := nf.grpcServer.Serve(lis)
		if err != nil {
			nf.log.Error(err, "Serving Device Plugin incoming requests failed.")
		}
	}()

	// Use connectWithRetry for the pluginEndpoint call
	conn, err = nf.connectWithRetry("unix:" + pluginEndpoint)
	if err != nil {
		return fmt.Errorf("resource %s unable to establish test connection with gRPC server: %v", resourceName, err)
	}
	nf.log.Info("Device plugin endpoint started serving:", "resourceName", resourceName)
	conn.Close()

	request := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     nf.socketFile,
		ResourceName: resourceName,
	}

	if _, err = client.Register(context.Background(), request); err != nil {
		return fmt.Errorf("unable to register resource %s with Kubelet: %v", resourceName, err)
	}
	nf.log.Info("Device plugin registered with Kubelet", "resourceName", resourceName)

	return nil
}

func (nf *nfResources) Start() error {
	err := nf.cleanup()
	if err != nil {
		return fmt.Errorf("failed to cleanup: %v", err)
	}

	err = nf.GetDevices()
	if err != nil {
		return fmt.Errorf("failed to get devices: %v", err)
	}

	err = nf.RegisterDevicePlugin()
	if err != nil {
		return fmt.Errorf("failed to register the device plugin: %v", err)
	}

	return nil
}

// connectWithRetry tries to establish a connection with the given endpoint, with retries.
func (nf *nfResources) connectWithRetry(endpoint string) (*grpc.ClientConn, error) {
	var conn *grpc.ClientConn
	var err error

	retryPolicy := `{
		"methodConfig": [{
		  "waitForReady": true,
		  "retryPolicy": {
			  "MaxAttempts": 40,
			  "InitialBackoff": "1s",
			  "MaxBackoff": "16s",
			  "BackoffMultiplier": 2.0,
			  "RetryableStatusCodes": [ "UNAVAILABLE" ]
		  }
		}]}`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err = grpc.DialContext(
		ctx,
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultServiceConfig(retryPolicy),
	)
	if err != nil {
		nf.log.Error(err, "Failed to establish connection with retry", "endpoint", endpoint)
		return nil, err
	}

	return conn, nil
}

func (nf *nfResources) ensureConnected() error {
	if nf.client != nil {
		return nil
	}
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.Dial("unix", addr)
		}),
	}

	conn, err := grpc.DialContext(context.Background(), VendorPluginSocketPath, dialOptions...)

	if err != nil {
		return fmt.Errorf("failed to connect to vendor plugin: %v", err)
	}
	nf.conn = conn

	nf.client = pb.NewDeviceServiceClient(conn)
	return nil
}

// func (nf *nfResources) Stop() error {
// 	fmt.Printf("Stopping Device Plugin gRPC server..")
// 	if nf.grpcServer == nil {
// 		return nil
// 	}

// 	nf.grpcServer.Stop()
// 	nf.grpcServer = nil

// 	return nf.cleanup()
// }

func (nf *nfResources) cleanup() error {
	pluginEndpoint := filepath.Join(pluginapi.DevicePluginPath, nf.socketFile)
	if err := os.Remove(pluginEndpoint); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

func (nf *nfResources) PreStartContainer(ctx context.Context, psRqt *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (nf *nfResources) GetDevicePluginOptions(ctx context.Context, empty *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

func NewGrpcPlugin() *nfResources {
	return &nfResources{
		log:        ctrl.Log.WithName("DevicePlugin"),
		devices:    make(map[string]pluginapi.Device),
		socketFile: pluginEndpoint,
	}
}
