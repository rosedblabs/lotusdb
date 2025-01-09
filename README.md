![lotusdb-logo.png](https://s2.loli.net/2024/05/18/Hkq6u2vhTgxbpEt.png)

## What is LotusDB

LotusDB is the most advanced key-value store written in Go, extremely fast, compatible with LSM tree and B+ tree, and optimization of badger and bbolt.

Key features:

* **Combine the advantages of LSM and B+ tree**
* **Fast read/write performance**
* **Much lower read and space amplification than typical LSM**

## Design Overview

![](https://github.com/lotusdblabs/lotusdb/blob/main/resource/img/design-overview.png)

## Getting Started
```go
package main

import (
	"github.com/lotusdblabs/lotusdb/v2"
)

func main() {
	// specify the options
	options := lotusdb.DefaultOptions
	options.DirPath = "/tmp/lotusdb_basic"

	// open a database
	db, err := lotusdb.Open(options)
	if err != nil {
		panic(err)
	}
	defer func() {
		_ = db.Close()
	}()

	// put a key
	err = db.Put([]byte("name"), []byte("lotusdb"))
	if err != nil {
		panic(err)
	}

	// get a key
	val, err := db.Get([]byte("name"))
	if err != nil {
		panic(err)
	}
	println(string(val))

	// delete a key
	err = db.Delete([]byte("name"))
	if err != nil {
		panic(err)
	}
}
```
see the [examples](https://github.com/lotusdblabs/lotusdb/tree/main/examples) for more details.

## Community

Welcome to join the [Slack channel](https://join.slack.com/t/rosedblabs/shared_invite/zt-19oj8ecqb-V02ycMV0BH1~Tn6tfeTz6A) and  [Discussions](https://github.com/lotusdblabs/lotusdb/discussions) to connect with LotusDB team members and other users.

If you are a Chinese user, you are also welcome to join our WeChat group, scan the QR code and you will be invited:

| <img src="https://i.loli.net/2021/05/06/tGTH7SXg8w95slA.jpg" width="200px" align="left"/> |
| ------------------------------------------------------------ |
