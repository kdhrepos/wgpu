module gitlab.com/akita/mgpusim

go 1.25.1

require (
	github.com/golang/mock v1.4.4
	github.com/onsi/ginkgo v1.16.4
	github.com/onsi/gomega v1.38.2
	github.com/rs/xid v1.6.0
	github.com/tebeka/atexit v0.3.0
	github.com/vbauerster/mpb/v4 v4.12.2
	gitlab.com/akita/akita v1.12.1
	gitlab.com/akita/dnn v0.5.4
	gitlab.com/akita/lmf v0.1.0
	// gitlab.com/akita/dnn v0.3.0
	gitlab.com/akita/mem v1.11.3
	gitlab.com/akita/noc v1.4.0
	gitlab.com/akita/protocol v0.1.0
	gitlab.com/akita/util v0.7.1
)

require github.com/spaolacci/murmur3 v1.1.0

require (
	github.com/VividCortex/ewma v1.1.1 // indirect
	github.com/acarl005/stripansi v0.0.0-20180116102854-5a71ef0e047d // indirect
	github.com/aws/aws-sdk-go v1.37.14 // indirect
	github.com/disintegration/imaging v1.6.2 // indirect
	github.com/fsnotify/fsnotify v1.4.9 // indirect
	github.com/go-sql-driver/mysql v1.5.0 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/golang/snappy v0.0.2 // indirect
	github.com/google/btree v1.0.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/klauspost/compress v1.11.7 // indirect
	github.com/nxadm/tail v1.4.8 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/xdg/scram v0.0.0-20180814205039-7eeb5667e42c // indirect
	github.com/xdg/stringprep v1.0.0 // indirect
	go.mongodb.org/mongo-driver v1.4.6 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/image v0.0.0-20210220032944-ac19c3e999fb // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
	golang.org/x/term v0.34.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	gonum.org/v1/gonum v0.8.2 // indirect
	gopkg.in/tomb.v1 v1.0.0-20141024135613-dd632973f1e7 // indirect
)

replace gitlab.com/akita/akita => ../akita

replace gitlab.com/akita/noc => ../noc

replace gitlab.com/akita/noc/networking => ../noc/networking

replace gitlab.com/akita/mem => ../mem

replace gitlab.com/akita/util => ../util


replace gitlab.com/akita/lmf => ../lmf

replace gitlab.com/akita/protocol => ../protocol

// replace gitlab.com/akita/dnn => ../dnn

// go 1.13
