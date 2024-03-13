package redis

import "github.com/redis/rueidis"

var client rueidis.Client

func Init(addresses []string) error {
	var err error
	client, err = rueidis.NewClient(rueidis.ClientOption{InitAddress: addresses})
	if err != nil {
		return err
	}
	defer client.Close()
	return nil
}

func GetClient() rueidis.Client {
	return client
}
