# Delta Table Differ

## User Story

Jenny the data engineer runs a new retention-and-column-remover ETL over multiple Delta Lake tables. To test the ETL before running on production, she uses lakeFS (obviously) and branches out to a dedicated branch `exp1`, and runs the ETL pointed at `exp1`.
The output is not what Jenny planned... The Delta Table is missing multiple columns and the number of rows just got bigger!
She would like to debug the ETL run. In other words, she would want to see the Delta table history changes inserted applied in branch `exp1` since the commit that she branched from.

---

## Goals and scope

1. For the MVP, support only Delta table diff.
2. The system should be open for different data comparison implementations.
3. The diff itself will consist of metadata changes only (computed using the operation history of the two tables) and, optionally, the change in the number of rows.
4. The diff will be a "two-dots" diff (like `git log branch1..branch2`). Basically showing the log changes that happened in the topic branch and not in the base branch.
5. UI: GUI only.
6. Reduce user friction as much as possible.

---

## Non-Goals and off-scope

1.  The Delta diff will be limited to the available Delta Log entries (the JSON files), which are, by default, retained
for [30 days](https://docs.databricks.com/delta/history.html#configure-data-retention-for-time-travel).
2. Log compaction isn't supported.
3. Delta Table diff will only be supported if the table stayed where it was, e.g. if we have a Delta table on path
`repo/branch1/delta/table` and it moved in a different branch to location `repo/branch2/delta/different/location/table`
then it won't be "diffable".

---

## High-Level design

### Considered architectures

##### Diff as part of the lakeFS server

Implement the diff functionality within lakeFS.  
Delta doesn't provide a Go SDK implementation, so we'll need to implement the Delta data model and interface ourselves, which is not something we aim to do.
In addition, this couples future diff implementations to Go (which might result in similar problems).

##### Diff as a standalone RESTful server

The diff implementation will be implemented in one of the available Delta Lake SDK languages and will be called RESTfully from lakeFS to get the diff results.
Users will run and manage the server and add communication details to the lakeFS configuration file.  
The problem with this approach is the friction added for the users. We intend to integrate a diff experience as seamlessly as possible, yet this solution is adding the overhead of managing another server alongside lakeFS.

##### Diff as bundled Rust/WebAssembly in lakeFS

Using the `delta-rs` package to build the differ, and bundling it into the lakeFS binary using `cgo` (to communicate with Rust's FFI) or a Web Assembly package that will use Go as the Web Assembly runtime/engine.  
Multiple languages can be compiled to WebAssembly which is a great benefit, yet the engines that are needed to run WebAssembly runtime in Go are implemented in other languages (not Go) and are unstable and raise compilation complexity and maintenance.

##### Diff as an external binary executable

We can trigger a diff binary from lakeFS using the `os.exec` package and get the output generated by that binary.
This is almost where we want to be, except that we'll need to somehow validate that the lakeFS server and the executable are using the same "API version" to communicate, so that the output of the binary would match the expected one from lakeFS. In addition, I would like to decrease the amount of deserialization needed to be implemented to interpret the returned DTO (maintainability and extensibility-wise).

### Chosen architecture

#### The Microkernel/Plugin Architecture

The Microkernel/Plugin architecture is composed of two entities: a single "core system" and multiple "plugins".  
In our case, the lakeFS server act as the core system, and the different diff implementations, including the Delta diff implementation, will act as plugins.  
We'll use `gRPC` as the transport protocol, which makes the language of choice almost immaterial (due to the use of protobufs as the transferred data format)
as long as it's self-contained (theoretically it can also be not system-runtime-dependent but then the cost will be an added requirement for running lakeFS- runtime support).

#### Plugin system consideration

* The plugin system should be flexible enough such that it won't impose a language restriction, so that in the future we could have lakeFS users take part in plugins creation (not restricted to diffs).
* The plugin system should support multiple OSs.
* The plugin system should be easy enough to write plugins.

#### Hashicorp Plugin system

Hashicorp's battle-tested [`go-plugin`](https://github.com/hashicorp/go-plugin) system is a plugin system over RPC (used in Terraform, Vault, Consul, and Packer).
Currently, it's only designed to work over a local network.  
Plugins can be written and consumed in almost every major language. This is achieved by supporting [gRPC](http://www.grpc.io/) as the communication protocol.
* The plugin system works by launching subprocesses and communicating over RPC (both `net/rpc` and `gRPC` are supported).
  A single connection is made between any plugin and the core process. For gRPC-based plugins, the HTTP2 protocol handles [connection multiplexing](https://freecontent.manning.com/animation-http-1-1-vs-http-2-vs-http-2-with-push/).
* The plugin and core system are separate processes, which means that a crash of a plugin won't cause the core system to crash (lakeFS in our case).

![Microkernel architecture overview](diagrams/microkernel-overview.png)
[(excalidraw file)](diagrams/microkernel-overview.excalidraw)

---

## Implementation

![Delta Diff flow](diagrams/delta-diff-flow.png)
[(excalidraw file)](diagrams/delta-diff-flow.excalidraw)

#### DiffService

The DiffService will be an internal component in lakeFS which will serve as the Core system.  
In order to realize which diff plugins are available, we shall use new manifest section and environment variables:

```yaml
diff:
  <diff_type>:
    plugin: <plugin_name_1>
plugins:
  <plugin_name_1>: <location to the 'plugin_name_1' plugin - full path to the binary needed to execute>
  <plugin_name_2>: <location to the 'plugin_name_1' plugin - full path to the binary needed to execute>    
```

1. `LAKEFS_DIFF_{DIFF_TYPE}_PLUGIN`
   The DiffService will use the name of plugin provided by this env var to load the binary and perform the diff. For instance,
   `LAKEFS_DIFF_DELTA_PLUGIN=delta_diff_plugin` will search for the binary path under the manifest path
   `plugins.delta_diff_plugin` or environment variable `LAKEFS_PLUGINS_DELTA_DIFF_PLUGIN`.
   If a given plugin path will not be found in the manifest or env var (e.g. `delta_diff_plugin` in the above example),
   the binary of a plugin with the same name will be searched under `~/.lakefs/plugins` (as the
   [plugins' default location](https://github.com/hashicorp/terraform/blob/main/plugins.go)).
2. `LAKEFS_PLUGINS_{PLUGIN_NAME}`
   This environment variable (and corresponding manifest path) will provide the path to the binary of a plugin named
   {PLUGIN_NAME}, for example, `LAKEFS_PLUGINS_DELTA_DIFF_PLUGIN=/path/to/delta/diff/binary`.

The **type** of diff will be sent as part of the request to lakeFS as specified [here](#API).
The communication between the DiffService and the plugins, as explained above, will be through the `go-plugin` package (`gRPC`).

#### Delta Diff Plugin

Implemented using [delta-rs](https://github.com/delta-io/delta-rs) (Rust), this plugin will perform the diff operation using table paths provided by the DiffService through a `gRPC` call.  
To query the Delta Table from lakeFS, the plugin will generate an S3 client (this is a constraint imposed by the `delta-rs` package) and send a request to lakeFS's S3 gateway.  

* The objects requested from lakeFS will only be the `_delta_log` JSON files, as they are the files that construct a table's history.
* We shall limit the number of returned objects to 1000 (each entry is ~500 bytes).<sup>[2](#future-plans)</sup>

_The diff algorithm:_
1. Get the table version of the base common ancestor.
2. Run a variation of the Delta [HISTORY command](https://docs.delta.io/latest/delta-utility.html#history-schema)<sup>[3](#future-plans)</sup>
(this variation starts from a given version number) on both table paths, starting from the version retrieved above.
    - If the left table doesn't include the base version (from point 1), start from the oldest version greater than the base.
3. Operate on the [returned "commitInfo" entry vector ](https://github.com/delta-io/delta-rs/blob/d444cdf7588503c1ebfceec90d4d2fadbd50a703/rust/src/delta.rs#L910)
starting from the **oldest** version of each history vector that is greater than the base table version: 
    1. If one table version is greater than the other table version, increase the smaller table's version until it gets to the same version of the other table.
    2. While there are more versions available && answer's vector size < 1001: 
        1. Create a hash for the entry based on fields: `timestamp`, `operation`, `operationParameters` , and `operationMetrics` values.
        2. Compare the hashes of the versions.
        3. If they **aren't equal**, add the "left" table's entry to the returned history list.
        4. Traverse one version forward in both vectors.
4. Return the history list.

### Authentication<sup>[4](#future-plans)</sup>

The `delta-rs` package generates an S3 client which will be used to communicate back to lakeFS (through the S3 gateway).  
In order for the S3 client to communicate with lakeFS (or S3 in general) it needs to pass an AWS Access Key Id and Secret access key.  
Since applicative credentials are not obligatory, the users that sent the request for a diff might not have such, and even if they have,
they cannot send them through the GUI (which is the UI we chose to implement this feature for the MVP).
To overcome this scenario, we'll use special diff credentials as follows:
1. User makes a Delta Diff request.
2. The DiffService checks if the user has "diff credentials" in the DB:
    1. If there are such credentials, it will use them.
    2. If there aren't such, it will generate the credentials and save them: `{AKIA: DIFF-<>, SAK: <>}`. The `DIFF` prefix will be used to identify "diff credentials".
3. The DiffService will pass the credentials to the Delta Diff plugin during the call as part of the `gRPC` call.

### API

- GET `/repositories/repo/{repo}/otf/refs/{left_ref}/diff/{right_ref}?table_path={path}&type={diff_type}`
    - Tagged as experimental
    - **Response**:  
        The response includes an array of operations from different versions of the specified table format.
        It has a general structure that enables formatting the different table format operation structs.
      - version:
        - type: string
        - description: the version/snapshot/transaction id of the operation.
      - timestamp (epoch):
        - type: long
        - description: operation's timestamp.
      - operation:
        - type: string
        - description: operation's name.
      - operationContent:
        - type: map
        - description: an operation content specific to the table format implemented.
      
      **Delta lake response example**:
      ```json
      [
           {
               "version": "1",
               "timestamp":1515491537026,
               "operation":"INSERT",
               "operationContent":{
                   "operationParameters": {
                      "mode":"Append",
                      "partitionBy":"[]"
                    }
          },
          ...
      ]
      ```
    
---

### Build & Package

#### Build

The Rust diff implementation will be located within the lakeFS repo. That way the protobuf will be shared between the 
"core" and "plugin" components easily. We will use `cargo` to build the artifacts for the different architectures.  

###### lakeFS development

The different plugins will have their own target in the `makefile` which will not be included as a (sub) dependency
of the `all` target. That way we'll not force lakeFS developers to include Rust (or any other plugin runtime) 
in their stack.
If developers want to develop new plugins, they'll have to include the necessary runtime in their environment.

* _Contribution guide_: It also means updating the contribution guide on how to update the makefile if a new plugin is added.

###### Docker

The docker file will build the plugins and will include Rust in the build part.

#### Package

1. We will package the binary within the released lakeFS + lakectl archive. It will be located under a `plugins` directory:
`plugins/diff/delta_v1`.
2. We will also update the Docker image to include the binary within it. The docker image is what clients should
use in order to get the "out-of-the-box" experience.

---

## Metrics

### Applicative Metrics

- Diff runtime
- The total number of versions read (per diff) - this is the number of Delta Log (JSON) files that were read from lakeFS by the plugin.
This is basically the size of a Delta Log. We can use it later on to optimize reading and understand an average size of 
Delta log.

### Business Statistics

- The number of unique requests by installation id- get the lower bound of the number of Delta Lake users.

---

## Future plans

1. Add a separate call to the plugin that merely checks that the plugin si willing to try to run on a location.
This allows a better UX(gray-out the button until you type in a good path), and will probably be helpful for performing 
auto-detection.
2. Support pagination.
3. The `delta-rs` HISTORY command is inefficient as it sends multiple requests to get all the Delta log entries. 
We might want to hold this information in our own model to make it easier for the diff (and possibly merge) to work.
4. This design's authentication method is temporary. It enlists two auth requirements that we might need to implement:
   1. Support tags/policies for credentials.
   2. Allow temporary credentials.
