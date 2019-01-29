//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//

package operator

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"

	admin "github.com/arangodb/kube-arangodb/pkg/admin"
	api "github.com/arangodb/kube-arangodb/pkg/apis/admin/v1alpha"
	"github.com/arangodb/kube-arangodb/pkg/util/k8sutil"

	watch "k8s.io/apimachinery/pkg/watch"
)

// WatchResource defines a resource to be watched
type WatchResource struct {
	// Name of the resource
	Name string
	// Type is the runtime type of the object
	Type runtime.Object
}

var resources = []WatchResource{
	{
		Name: api.ArangoDatabaseResourcePlural,
		Type: &api.ArangoDatabase{},
	},
	{
		Name: api.ArangoUserResourcePlural,
		Type: &api.ArangoUser{},
	},
	{
		Name: api.ArangoCollectionResourcePlural,
		Type: &api.ArangoCollection{},
	},
}

func (o *Operator) watchDatabaseResources(stop <-chan struct{}) {
	for _, r := range resources {
		rw := k8sutil.NewResourceWatcher(
			o.log,
			o.Dependencies.CRCli.DatabaseadminV1alpha().RESTClient(),
			r.Name,
			o.Config.Namespace,
			r.Type,
			cache.ResourceEventHandlerFuncs{
				AddFunc:    o.onAddDatabaseAdminResource,
				UpdateFunc: o.onUpdateDatabaseAdminResource,
				DeleteFunc: o.onDeleteDatabaseAdminResource,
			})
		go rw.Run(stop)
	}
}

func (o *Operator) onAddDatabaseAdminResource(obj interface{}) {
	fmt.Println("onAddDatabaseAdminResource")
	o.databaseAdmin.EventChannel <- watch.Event{
		Type:   watch.Added,
		Object: obj.(runtime.Object),
	}
}

func (o *Operator) onUpdateDatabaseAdminResource(old, new interface{}) {
	fmt.Println("onUpdateDatabaseAdminResource")
	o.databaseAdmin.EventChannel <- watch.Event{
		Type:   watch.Modified,
		Object: new.(runtime.Object),
	}
}

func (o *Operator) onDeleteDatabaseAdminResource(obj interface{}) {
	fmt.Println("onDeleteDatabaseAdminResource")
	o.databaseAdmin.EventChannel <- watch.Event{
		Type:   watch.Deleted,
		Object: obj.(runtime.Object),
	}
}

func (o *Operator) runDatabaseAdmin(stop <-chan struct{}) {
	o.databaseAdmin = admin.NewDatabaseAdmin(o.Namespace, admin.Dependencies{
		Log:                o.Dependencies.LogService.MustGetLogger("databaseadmin"),
		KubeCli:            o.Dependencies.KubeCli,
		DatabaseAdminCRCli: o.Dependencies.CRCli,
		EventRecorder:      o.Dependencies.EventRecorder,
	})
	o.watchDatabaseResources(stop)
	o.DatabaseAdminProbe.SetReady()

	o.databaseAdmin.Run(stop)
}
