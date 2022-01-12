  
# gclone

A modified version of the [rclone](//github.com/rclone/rclone)

Implement [donwa/gclone](https://github.com/donwa/gclone) to sync with rclone version

Provide dynamic replacement sa file support for google drive operation


## BUILD

### Prepare

```
#Windows cgo
WinFsp, gcc (e.g. from Mingw-builds)

#macOS
FUSE for macOS, command line tools

#Linux
libfuse-dev, gcc
```

### Step
- _(optional)_ install [cgofuse](https://github.com/billziss-gh/cgofuse)
- build
  ```
  go build -v -tags 'cmount' gclone.go
  ```

### Check

```
./gclone version
```

> if need `mount` function, cgofuse is required, 

## Instructions 
### 1.service_account_file_path Configuration   
add `service_account_file_path` Configuration.For dynamic replacement service_account_file(sa file). Replace configuration when `rateLimitExceeded` error occurs
`rclone.conf` example:  
```
[gc]
type = drive  
scope = drive  
service_account_file = /root/accounts/1.json  
service_account_file_path = /root/accounts/  
root_folder_id = root  
```
`/root/accounts/` Folder contains multiple access and edit permissions ***service account file(*.json)***.  
  
### 2.Support incoming id
If the original rclone is across team disks or shared folders, multiple configuration drive letters are required for operation.
gclone supports incoming id operation
```
gclone copy gc:{folde_id1} gc:{folde_id2}  --drive-server-side-across-configs
```
folde_id1 can be:Common directory, shared directory, team disk. 
  
```
gclone copy gc:{folde_id1} gc:{folde_id2}/media/  --drive-server-side-across-configs

```

```
gclone copy gc:{share_fiel_id} gc:{folde_id2}  --drive-server-side-across-configs
```

### 3.Support command line option `--drive-service-account-file-path`

```
gclone copy gc:{share_fiel_id} gc:{folde_id2} --drive-service-account-file-path=${SOMEWHERE_STORE_SAs}
```
  
## CAVEATS

Creating Service Accounts (SAs) allows you to bypass some of Google's quotas. Tools like autorclone and clonebot (gclone) automatically rotate SAs for continuous multi-terabyte file transfer.

> Quotas SAs **CAN** bypass:

* Google 'copy/upload' quota (750GB/account/day)
* Google 'download' quota (10TB/account/day)

> Quotas SAs **CANNOT** bypass:

* Google Shared Drive quota (~20TB/drive/day)
* Google file owner quota (~2TB/day)
