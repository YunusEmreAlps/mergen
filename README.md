# mergenc

`mergenc`, Firecracker microVM'lerini bir kontrol düzlemi altında yönetmek
için tasarlanmış hafif bir CLI aracıdır. Template kernel ve rootfs
görüntüleri `images/` altında tutulur; her yeni VM için bu dosyaların kopyası
`volumes/<vm-adı>/` altında oluşturulur. Her volume dizininde ayrıca
`config.json`, `firecracker.log`, `firecracker.sock` ve `firecracker.pid`
dosyaları yer alır.

## Gereksinimler

- KVM etkin bir Linux ana makine
- Docker veya en azından hazır bir `docker0` köprüsü (ya da elle belirteceğiniz başka bir bridge)
- `ip`, `sysctl` ve `firecracker` komutlarının PATH üzerinde olması
- Ağ arabirimleri üzerinde değişiklik yapabilmek için root yetkisi (komutları `sudo` ile çalıştırmanız gerekebilir)

İlk çekirdek ve rootfs dosyalarını almak için proje kökünde şunu çalıştırın:

```bash
./pre-run.sh
```

Bu komut `images/` klasörüne `vmlinux.bin` ve `rootfs.ext4` dosyalarını
indirir. VM oluşturulurken bu dosyaların kopyaları `volumes/<vm-adı>/`
altına taşınır.

## Derleme

```bash
go build -o mergenc ./cmd/mergenc
```

## Hızlı Başlangıç

Docker köprüsünü kullanan otomatik ağ kurulumu ile bir microVM başlatın:

```bash
# images/{vmlinux.bin,rootfs.ext4} dosyalarını önceden yerleştirin

# yeni VM için volume ve ağ hazırlığı
sudo ./mergenc create demo \
  --guest-ip 172.17.0.35 \
  --gateway-ip 172.17.0.1 \
  --netmask 255.255.255.0

# Firecracker sürecini başlat
sudo ./mergenc start demo

# Graceful shutdown (Ctrl+Alt+Del send)
sudo ./mergenc stop demo

# Tüm artıkları temizle (socket, tap, volume dizini)
sudo ./mergenc delete demo
```

`create` komutu TAP arayüzünü oluşturur, `docker0` köprüsüne bağlar ve
gerekirse MAC adresini otomatik üretir. Firecracker konfigürasyonu aynı
dizinde `config.json` olarak saklanır. `start` komutu Firecracker sürecini
bağımsız bir işlem olarak başlatır ve API üzerinden sürücü/ağ yapılandırması
geçtikten sonra `InstanceStart` çağrısını yapar. `stop` komutu mevcut soket
üzerinden `SendCtrlAltDel` göndererek misafirin kapanmasını bekler. `delete`
komutu VM kapalı değilse önce durdurur, ardından oluşturulmuş TAP arayüzünü ve
volume dizinini kaldırır.
