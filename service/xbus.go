package service

import (
	"crypto/tls"
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/golang/glog"
	"github.com/infrmods/xbus/comm"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	EtcdEndpoints []string      `default:"[\"127.0.0.1:2378\"]" yaml:"etcd_endpoints"`
	EtcdTimeout   time.Duration `default:"5s" yaml:"etcd_timeout"`
	EtcdTLS       *tls.Config   `yaml:"etcd_tls"`
	KeyPrefix     string        `default:"/services/"`
}

type XBus struct {
	config     Config
	etcdClient *clientv3.Client
}

func NewXBus(config *Config) *XBus {
	xbus := &XBus{config: *config}
	if strings.HasSuffix(xbus.config.KeyPrefix, "/") {
		xbus.config.KeyPrefix = xbus.config.KeyPrefix[:len(xbus.config.KeyPrefix)-1]
	}
	return xbus
}

func (xbus *XBus) Init() (err error) {
	etcd_config := clientv3.Config{
		Endpoints:   xbus.config.EtcdEndpoints,
		DialTimeout: xbus.config.EtcdTimeout,
		TLS:         xbus.config.EtcdTLS}
	if xbus.etcdClient, err = clientv3.New(etcd_config); err == nil {
		return nil
	} else {
		return fmt.Errorf("create etcd clientv3 fail(%v)", err)
	}
}

var rValidName = regexp.MustCompile(`(?i)[a-z][a-z0-9_.]{5,}`)
var rValidVersion = regexp.MustCompile(`(?i)[a-z0-9][a-z0-9_.]*`)

func checkNameVersion(name, version string) error {
	if !rValidName.MatchString(name) {
		return comm.NewError(comm.EcodeInvalidName, "")
	}
	if !rValidVersion.MatchString(version) {
		return comm.NewError(comm.EcodeInvalidVersion, "")
	}
	return nil
}

var rValidServiceId = regexp.MustCompile(`(?i)[a-f0-9]+`)

func checkServiceId(id string) error {
	if !rValidServiceId.MatchString(id) {
		return comm.NewError(comm.EcodeInvalidServiceId, "")
	}
	return nil
}

func (xbus *XBus) Plug(ctx context.Context, name, version string,
	ttl time.Duration, endpoint *comm.ServiceEndpoint) (string, clientv3.LeaseID, error) {
	if err := checkNameVersion(name, version); err != nil {
		return "", 0, err
	}
	if endpoint.Type == "" {
		return "", 0, comm.NewError(comm.EcodeInvalidEndpoint, "missing type")
	}
	if endpoint.Address == "" {
		return "", 0, comm.NewError(comm.EcodeInvalidEndpoint, "missing address")
	}
	data, err := endpoint.Marshal()
	if err != nil {
		return "", 0, err
	}
	return xbus.newUniqueNode(ctx, ttl, xbus.etcdKeyPrefix(name, version), string(data))
}

func (xbus *XBus) Unplug(ctx context.Context, name, version, id string) error {
	if err := checkNameVersion(name, version); err != nil {
		return err
	}
	if err := checkServiceId(id); err != nil {
		return err
	}
	if _, err := xbus.etcdClient.Delete(ctx, xbus.etcdKey(name, version, id)); err != nil {
		glog.Errorf("delete key(%s) fail: %v", xbus.etcdKey(name, version, id), err)
		return comm.NewError(comm.EcodeSystemError, "delete key fail")
	}
	return nil
}

func (xbus *XBus) Update(ctx context.Context, name, version, id string, endpoint *comm.ServiceEndpoint) error {
	if err := checkNameVersion(name, version); err != nil {
		return err
	}
	if err := checkServiceId(id); err != nil {
		return err
	}
	key := xbus.etcdKey(name, version, id)
	data, err := endpoint.Marshal()
	if err != nil {
		return err
	}

	resp, err := xbus.etcdClient.Txn(
		ctx,
	).If(
		clientv3.Compare(clientv3.Version(key), ">", 0),
	).Then(clientv3.OpPut(key, string(data))).Commit()
	if err == nil {
		if resp.Succeeded {
			return nil
		}
		return comm.NewError(comm.EcodeNotFound, "")
	} else {
		glog.Errorf("tnx update(%s) fail: %v", key, err)
		return comm.NewError(comm.EcodeSystemError, "")
	}
}

func (xbus *XBus) KeepAlive(ctx context.Context, name, version, id string, keepId clientv3.LeaseID) error {
	if err := checkNameVersion(name, version); err != nil {
		return err
	}
	if err := checkServiceId(id); err != nil {
		return err
	}
	if _, err := xbus.etcdClient.Lease.KeepAliveOnce(ctx, keepId); err != nil {
		if grpc.Code(err) == codes.NotFound {
			return comm.NewError(comm.EcodeNotFound, "")
		}
		glog.Errorf("KeepAliveOnce(%d) fail: %v", keepId, err)
		return comm.NewError(comm.EcodeSystemError, "")
	}
	return nil
}

func (xbus *XBus) Query(ctx context.Context, name, version string) ([]comm.ServiceEndpoint, int64, error) {
	if err := checkNameVersion(name, version); err != nil {
		return nil, 0, err
	}
	key := xbus.etcdKeyPrefix(name, version)
	return xbus.query(ctx, key)
}

func (xbus *XBus) query(ctx context.Context, key string) ([]comm.ServiceEndpoint, int64, error) {
	if resp, err := xbus.etcdClient.Get(ctx, key); err == nil {
		if endpoints, err := xbus.makeEndpoints(resp.Kvs); err == nil {
			return endpoints, resp.Header.Revision, nil
		} else {
			return nil, 0, err
		}
	} else {
		if grpc.Code(err) == codes.NotFound {
			return nil, 0, comm.NewError(comm.EcodeNotFound, "")
		}
		glog.Errorf("Query(%s) fail: %v", key, err)
		return nil, 0, comm.NewError(comm.EcodeSystemError, "")
	}
}

func (xbus *XBus) Watch(ctx context.Context, name, version string,
	revision int64, timeout time.Duration) ([]comm.ServiceEndpoint, int64, error) {
	if err := checkNameVersion(name, version); err != nil {
		return nil, 0, err
	}
	key := xbus.etcdKeyPrefix(name, version)
	watcher := clientv3.NewWatcher(xbus.etcdClient)
	defer watcher.Close()
	tCtx, cancelFunc := context.WithTimeout(ctx, timeout)
	defer cancelFunc()
	watchCh := watcher.Watch(tCtx, key, clientv3.WithRev(revision))
	resp := <-watchCh
	if !resp.Canceled {
		return xbus.query(tCtx, key)
	}
	return nil, 0, nil
}
