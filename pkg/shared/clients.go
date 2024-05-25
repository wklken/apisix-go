package shared

import (
	"fmt"
	"sync"
)

// a sync.Map to store all the clients

var globalClients sync.Map

func LoadOrStoreClient(pluginName string, uid *ConfigUID, client interface{}) (actual any) {
	key := fmt.Sprintf("%s:%s", pluginName, uid.String())
	actual, _ = globalClients.LoadOrStore(key, client)
	return
}

// func RegisterClient(pluginName, uid string, client interface{}) {
// 	globalClients.Store(key, client)
// }

// func GetClient(pluginName, uid string) (interface{}, bool) {
// 	return globalClients.Load(uid)
// }
