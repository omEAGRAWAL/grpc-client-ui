package main

import (
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var (
	descriptorSetPath = "./compiled.protoset"
	tempProtoDir      = "./uploaded_protos"
)
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var importPaths []string
var descriptorSet descriptorpb.FileDescriptorSet

func main() {
	r := gin.Default()
	r.POST("/load-proto", handleProtoUpload)
	r.GET("/services", handleListServices)
	r.POST("/add-import", handleAddImportPath)
	r.POST("/grpc-call", handleGrpcCall)
	r.POST("/grpc-stream", handleGrpcStreamHTTP)
	r.StaticFile("/", "./ui.html")

	r.Run(":8081")
}

func handleProtoUpload(c *gin.Context) {
	file, err := c.FormFile("proto")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No file uploaded"})
		return
	}

	if err := os.MkdirAll(tempProtoDir, os.ModePerm); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not create upload dir"})
		return
	}

	savedPath := filepath.Join(tempProtoDir, file.Filename)
	if err := c.SaveUploadedFile(file, savedPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not save file"})
		return
	}

	args := []string{}
	for _, path := range importPaths {
		args = append(args, "--proto_path="+path)
	}
	args = append(args, "--proto_path="+tempProtoDir, "--descriptor_set_out="+descriptorSetPath, "--include_imports", savedPath)

	cmd := exec.Command("protoc", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to compile .proto with protoc"})
		return
	}

	data, err := os.ReadFile(descriptorSetPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not read descriptor set"})
		return
	}
	if err := proto.Unmarshal(data, &descriptorSet); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse descriptor set"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Proto uploaded and compiled successfully"})
}

func handleAddImportPath(c *gin.Context) {
	var req struct {
		Path string `json:"path"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}
	if req.Path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Path cannot be empty"})
		return
	}
	importPaths = append(importPaths, req.Path)
	c.JSON(http.StatusOK, gin.H{"message": "Import path added", "paths": importPaths})
}

func handleListServices(c *gin.Context) {
	services := []map[string]interface{}{}
	for _, file := range descriptorSet.File {
		fdesc, err := protodesc.NewFile(file, nil)
		if err != nil {
			continue
		}
		for i := 0; i < fdesc.Services().Len(); i++ {
			svc := fdesc.Services().Get(i)
			methods := []string{}
			for j := 0; j < svc.Methods().Len(); j++ {
				methods = append(methods, string(svc.Methods().Get(j).Name()))
			}
			services = append(services, map[string]interface{}{
				"service": string(svc.FullName()),
				"methods": methods,
			})
		}
	}
	c.JSON(http.StatusOK, services)
}

func dynamicNewMessage(desc protoreflect.MessageDescriptor) (proto.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(desc.FullName())
	if err != nil {
		return nil, err
	}
	return mt.New().Interface(), nil
}
func handleGrpcCall(c *gin.Context) {
	var req struct {
		Target     string          `json:"target"`
		Service    string          `json:"service"`
		Method     string          `json:"method"`
		Message    json.RawMessage `json:"message"`
		TimeoutSec int             `json:"timeout_sec"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	conn, err := grpc.Dial(req.Target, grpc.WithInsecure())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to connect to gRPC server"})
		return
	}
	defer conn.Close()

	fileDesc, err := desc.CreateFileDescriptorFromSet(&descriptorSet)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Descriptor conversion failed"})
		return
	}

	var svc *desc.ServiceDescriptor
	var mtd *desc.MethodDescriptor

	for _, s := range fileDesc.GetServices() {
		if s.GetFullyQualifiedName() == req.Service {
			svc = s
			mtd = s.FindMethodByName(req.Method)
			break
		}
	}
	if svc == nil || mtd == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Service or Method not found"})
		return
	}

	inputMsg := dynamic.NewMessage(mtd.GetInputType())
	if err := inputMsg.UnmarshalJSON(req.Message); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSec)*time.Second)
	defer cancel()

	resMsg := dynamic.NewMessage(mtd.GetOutputType())

	err = conn.Invoke(ctx, "/"+req.Service+"/"+req.Method, inputMsg, resMsg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "RPC failed: " + err.Error()})
		return
	}

	jsonBytes, _ := resMsg.MarshalJSON()
	c.Data(http.StatusOK, "application/json", jsonBytes)
}
func handleGrpcStreamHTTP(c *gin.Context) {
	var req struct {
		Target     string          `json:"target"`
		Service    string          `json:"service"`
		Method     string          `json:"method"`
		Message    json.RawMessage `json:"message"`
		TimeoutSec int             `json:"timeout_sec"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
		return
	}

	grpcConn, err := grpc.Dial(req.Target, grpc.WithInsecure())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to connect to gRPC server"})
		return
	}
	defer grpcConn.Close()

	fileDesc, err := desc.CreateFileDescriptorFromSet(&descriptorSet)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Descriptor parsing failed"})
		return
	}

	var svc *desc.ServiceDescriptor
	var mtd *desc.MethodDescriptor
	for _, s := range fileDesc.GetServices() {
		if s.GetFullyQualifiedName() == req.Service {
			svc = s
			mtd = s.FindMethodByName(req.Method)
			break
		}
	}
	if svc == nil || mtd == nil || !mtd.IsServerStreaming() {
		c.JSON(http.StatusNotFound, gin.H{"error": "Streaming method not found"})
		return
	}

	inputMsg := dynamic.NewMessage(mtd.GetInputType())
	if err := inputMsg.UnmarshalJSON(req.Message); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message format"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSec)*time.Second)
	defer cancel()

	streamDesc := &grpc.StreamDesc{
		StreamName:    mtd.GetName(),
		ServerStreams: true,
		ClientStreams: false,
	}

	stream, err := grpc.NewClientStream(ctx, streamDesc, grpcConn, "/"+req.Service+"/"+req.Method)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open stream: " + err.Error()})
		return
	}

	if err := stream.SendMsg(inputMsg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "SendMsg failed: " + err.Error()})
		return
	}
	stream.CloseSend()

	// Set headers for streaming
	c.Writer.Header().Set("Content-Type", "application/x-ndjson")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// Stream each message
	for {
		resp := dynamic.NewMessage(mtd.GetOutputType())
		if err := stream.RecvMsg(resp); err != nil {
			fmt.Fprintf(c.Writer, `{"end":true,"error":"%s"}`+"\n", err.Error())
			flusher.Flush()
			return
		}
		data, _ := resp.MarshalJSON()
		c.Writer.Write(data)
		c.Writer.Write([]byte("\n")) // newline delimited
		flusher.Flush()
	}
}

func HandleGrpcStreamServer() {

	// This function is not used in the current implementation
	// but can be used to handle gRPC streaming server calls.
	// You can implement your own logic here if needed.
	// For example, you can create a gRPC server and register
	// your service handlers to handle streaming calls.
	// This is a placeholder for future implementation.
	// You can implement your own logic here if needed.
	//WRITE CODE

}
