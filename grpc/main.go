package main

import (
	"context"
	"fmt"

	"go.starlark.net/lib/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	client, err := grpc.NewClient("10.89.0.13:8081", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(err)
	}
	defer client.Close()
	client.Connect()
	fmt.Printf("Connection state: %v\n", client.GetState())
	err = client.Invoke(context.Background(), "/repository.RepoServerService/TestRepository", proto.Message{}, "")
	if err != nil {
		panic(err)
	}
}
