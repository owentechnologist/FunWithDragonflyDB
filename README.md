# FunWithDragonflyDB
A place to capture sample code and such

# Example 1: 
# This example uses CockroachDB as the system of record (as well as the source of changes propagated through a simple listener program to DragonFlyDB)
![IMG of cdc flow](./cdcJSONSearch/cdcJSONSearch.png)

# It involves:
## 1. Capturing changes from CRDB (Cockroach Database) and writing them as JSON objects into DragonFlyDB
## 2. Then Searching DRagonflyDB for JSON objects (vehicles) of a particular color
## 3. Then updating one record in CockroachDB and searching DragonFlyDB again to see that the CDC has updated the searchable cache as well

# CD to the cdcJSONSearch folder
```
cd cdcJSONSearch
```

* Ensure you have Go installed
```
brew install go
```

** also install the redis go library:
```
go mod download github.com/redis/go-redis/v9

go get github.com/redis/go-redis/v9@v9.7.3
```

* Install DragonFlyDB
```
brew install dragonflydb
```

## To run DragonflyDB in a container you can use docker or podman:
* Install podman
```
brew install podman
```
* create a VM large enough to build cool stuff
```
podman machine init dragonfly --cpus 5 --memory 8192 --disk-size 20
```
* start a vm to use with DragonflyDB
```
podman machine start dragonfly
```
* start a containerized local instance of DragonFlyDB
```
podman --connection dragonfly run -p 6379:6379 --ulimit memlock=-1 docker.dragonflydb.io/dragonflydb/dragonfly &
```

## To connect on the command line use the redis cli
* install redis and the redis-cli
```
brew install redis
```
* start the redis-cli (it will use port 6379 by default)
```
redis-cli
```
* in the redis-cli shell: create a Search index to be used once we populate DragonFlyDB with JSON objects
```
FT.CREATE idx_vehicles ON JSON PREFIX 1 vehicle: SCHEMA $.after.city AS CURRENT_CITY TEXT $.after.current_location AS STREET_ADDRESS TEXT $.after.status AS CURRENT_STATUS TAG $.after.type AS VEHICLE_TYPE TAG $.after.ext.color AS COLOR TAG $.after.ext.brand AS BRAND TAG
```

* from the redis-cli interactive shell do:
```
dbsize
```

* Ensure you have CockroachDB installed:
```
brew install cockroachdb/tap/cockroach
```
## Start a local demo CRDB cluster with the demo movr app: 
* running cockroach demo starts an interactive session with the movr database:
```
cockroach demo --with-load --insecure
```
<details><summary>Expected Output:</summary>
<p>

```bash
#
# Welcome to the CockroachDB demo database!
#
# You are connected to a temporary, in-memory CockroachDB cluster of 1 node.
#
# This demo session will send telemetry to Cockroach Labs in the background.
# To disable this behavior, set the environment variable
# COCKROACH_SKIP_ENABLING_DIAGNOSTIC_REPORTING=true.
#
# Beginning initialization of the movr dataset, please wait...
#
# The cluster has been preloaded with the "movr" dataset
# (MovR is a fictional vehicle sharing company).
#
# Reminder: your changes to data stored in the demo session will not be saved!
#
# If you wish to access this demo cluster using another tool, you will need
# the following details:
#
#   - Connection parameters:
#      (webui)    http://127.0.0.1:8080
#      (cli)      cockroach sql --insecure -d movr
#      (sql)      postgresql://root@127.0.0.1:26257/movr?sslmode=disable
#   
# Server version: CockroachDB CCL v26.1.3 (aarch64-apple-darwin21.2, built 2026/04/16 16:55:46, go1.25.5) (same version as client)
# Cluster ID: 0349e893-3913-4315-aa77-ebf5193ec936
# Organization: Cockroach Demo
#
# Enter \? for a brief introduction.
#
root@127.0.0.1:26257/movr>
```
</p>
</details>

## Start the change feed for the vehicles table:
```
demo@127.0.0.1:26257/movr> CREATE CHANGEFEED FOR TABLE movr.vehicles INTO 'webhook-https://localhost:3000?insecure_tls_skip_verify=true' WITH updated; 
```

## Now we are ready to start the cdc_listener
*  Ensure you can execute the shell script:
in this project's cdcJSONSearch directory:

```
chmod 755 start_cdc_listener.go
```

* start the cdc listener:

```
./start_cdc_listener.sh
```

## You should see a periodic dump of JSON in the program terminal output and if you query dragonfly you will see the JSON objects now exist:

* Check for new objects in the cache by doing this from the redis-cli interactive shell:

```
dbsize
```

* Using the redis-cli Query for all blue vehicles:

```
FT.SEARCH idx_vehicles "@COLOR:{blue}" return 2 CURRENT_STATUS COLOR
```
<details><summary>Expected Output:</summary>
<p>

```bash
 1) (integer) 5
 2) "vehicle:dddddddd-dddd-4000-8000-00000000000d"
 3) 1) "COLOR"
    2) "blue"
    3) "CURRENT_STATUS"
    4) "in_use"
 4) "vehicle:33333333-3333-4400-8000-000000000003"
 5) 1) "COLOR"
    2) "blue"
    3) "CURRENT_STATUS"
    4) "in_use"
 6) "vehicle:55555555-5555-4400-8000-000000000005"
 7) 1) "COLOR"
    2) "blue"
    3) "CURRENT_STATUS"
    4) "in_use"
 8) "vehicle:aaaaaaaa-aaaa-4800-8000-00000000000a"
 9) 1) "COLOR"
    2) "blue"
    3) "CURRENT_STATUS"
    4) "in_use"
10) "vehicle:eeeeeeee-eeee-4000-8000-00000000000e"
11) 1) "COLOR"
    2) "blue"
    3) "CURRENT_STATUS"
    4) "in_use"
```
</p>
</details>


## Showcase CDC in action again:

* Using the CRDB interactive shell (connected to the movr database) Query the current state of one vehicle record:

```
demo@127.0.0.1:26257/movr>
SELECT * FROM VEHICLES WHERE id='dddddddd-dddd-4000-8000-00000000000d';
```
<details><summary>Expected Output:</summary>
<p>

```bash
                   id                  | city  | type |               owner_id               |    creation_time    | status |    current_location    |                 ext
---------------------------------------+-------+------+--------------------------------------+---------------------+--------+------------------------+---------------------------------------
  dddddddd-dddd-4000-8000-00000000000d | paris | bike | d1eb851e-b851-4800-8000-000000000029 | 2019-01-02 03:04:05 | in_use | 35426 Jordan Mountains | {"brand": "Merida", "color": "black"}
(1 row)

Time: 6ms total (execution 6ms / network 0ms)
```
</p>
</details>

* Using the CRDB interactive shell (connected to the movr database) update a record so the vehicle is now blue:

```
demo@127.0.0.1:26257/movr> UPDATE VEHICLES SET ext=jsonb_set(ext, '{color}', '"brown"') WHERE id = 'dddddddd-dddd-4000-8000-00000000000d';
```

## Back to the redis-cli to check that the CDC update was effective:


```
FT.SEARCH idx_vehicles "@COLOR:{brown}" return 2 CURRENT_STATUS COLOR
```

<details><summary>Expected Output:</summary>
<p>

```bash
1) (integer) 1
2) "vehicle:dddddddd-dddd-4000-8000-00000000000d"
3) 1) "COLOR"
   2) "brown"
   3) "CURRENT_STATUS"
   4) "in_use"
```
</p>
</details>

## Look at the latency for such a query against local DragonflyDB:
* execute multi/time/command/time/execute to see the time taken for the command
* subtract the first time from the second time to see how many microseconds it took

START_TIME:
1778176857 <-- seconds since epoch as known to DragonflyDB
247000 <-- microseconds after the last measured second in above measure

END_TIME:
1778176857 <-- seconds since epoch as known to DragonflyDB
247000 <-- microseconds after the last measured second in above measure

<details><summary>Sample Commands and Output:</summary>
<p>

```bash

127.0.0.1:6379> multi
OK
127.0.0.1:6379(TX)> time
QUEUED
127.0.0.1:6379(TX)> FT.SEARCH idx_vehicles "@COLOR:{silver | brown | blue | red}" SORTBY COLOR return 2 CURRENT_STATUS COLOR LIMIT 0 5
QUEUED
127.0.0.1:6379(TX)> time
QUEUED
127.0.0.1:6379(TX)> exec
1) 1) (integer) 1778182831
   2) (integer) 133000
2)  1) (integer) 132
    2) "vehicle:71735a29-667f-4c7d-a336-0b2fe83caaa7"
    3) 1) "COLOR"
       2) "blue"
       3) "CURRENT_STATUS"
       4) "lost"
    4) "vehicle:f112f454-d0b8-4d64-9f82-3f18fc366479"
    5) 1) "COLOR"
       2) "blue"
       3) "CURRENT_STATUS"
       4) "available"
    6) "vehicle:4ac4703c-cc93-49f7-bf50-3a216109e47d"
    7) 1) "COLOR"
       2) "blue"
       3) "CURRENT_STATUS"
       4) "in_use"
    8) "vehicle:12191b7d-123a-4d04-959f-1712d99d7396"
    9) 1) "COLOR"
       2) "blue"
       3) "CURRENT_STATUS"
       4) "in_use"
   10) "vehicle:a3be738f-44a6-4edb-adb7-e3ca84f72475"
   11) 1) "COLOR"
       2) "blue"
       3) "CURRENT_STATUS"
       4) "available"
3) 1) (integer) 1778182831
   2) (integer) 133000
```
</p>
</details>


