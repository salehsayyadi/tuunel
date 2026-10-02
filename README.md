<div dir="rtl">

# tuunel — تونل لایه ۳ بین سرور ایران و خارج

`tuunel` یک تونل لایه ۳ (TUN) برای لینوکس است که بین دو سرور یک اینترفیس `tun0` می‌سازد
و ترافیک را رمزنگاری‌شده (Noise) از روی چند پروتکل منتقل می‌کند:
**TCP، UDP، QUIC، QUIC-Datagram، WSS** و **ICMP (آزمایشی)**.
اگر یکی از پروتکل‌ها قطع یا فیلتر شود، تونل خودکار و بدون ری‌استارت روی پروتکل بعدی می‌رود.

> ⚠️ پروژه هنوز نسخهٔ پیش از ۱.۰ است و روی دو سرور واقعی ایران/خارج تست کامل نشده.
> قبل از استفادهٔ جدی، روی سرور تستی امتحان کنید.

---

## پیش‌نیازها

- دو سرور لینوکس (Ubuntu 22.04/24.04 یا Debian 12 یا Alma/Rocky 9)، معماری amd64 یا arm64
- دسترسی root (یا sudo)
- فعال بودن TUN روی سرور (در اکثر VPSها فعال است؛ اگر نبود از پنل سرور TUN/TAP را روشن کنید)
- نصب بودن `curl`:

```bash
sudo apt update && sudo apt install -y curl      # Ubuntu / Debian
# یا
sudo dnf install -y curl                         # Alma / Rocky
```

## نقش سرورها

| سرور | نقش در نصب | کار | آی‌پی داخل تونل |
|---|---|---|---|
| **ایران** | `edge` | منتظر اتصال می‌ماند (پورت‌ها را باز می‌کند) | `10.200.0.1` |
| **خارج** | `remote` | به سرور ایران وصل می‌شود | `10.200.0.2` |

پورت‌هایی که **فقط روی سرور ایران** باید باز باشند:
`443/tcp`، `443/udp`، `8443/tcp`، `51900/udp`.
روی سرور خارج نیازی به باز کردن پورت نیست.

---

## نصب مرحله به مرحله

دستور نصب رسمی (تک‌خطی):

```text
https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh
```

### مرحلهٔ ۱ — نصب روی سرور ایران

روی سرور **ایران** بزنید:

```bash
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --role=edge
```

در پایان، نصاب یک **کلید عمومی (PUBLIC KEY)** ۴۴ کاراکتری چاپ می‌کند. آن را کپی کنید
(این را «کلید ایران» می‌نامیم). هر وقت لازم شد دوباره ببینیدش:

```bash
cat /etc/tuunel/node.pub
```

اگر فایروال فعال است، پورت‌ها را باز کنید (مثال ufw):

```bash
sudo ufw allow 443/tcp && sudo ufw allow 443/udp && sudo ufw allow 8443/tcp && sudo ufw allow 51900/udp
```

> اگر پورت 443 روی سرور ایران قبلاً استفاده می‌شود (مثلاً وب‌سرور یا پنل)، در **هر دو**
> سرور گزینهٔ `--ports=tcp=2083,quic=2083,wss=2087,udp=51900` را به دستور نصب اضافه کنید
> (اعداد دلخواه).

### مرحلهٔ ۲ — نصب روی سرور خارج

روی سرور **خارج** بزنید؛ `IRAN_IP` را با آی‌پی سرور ایران و `IRAN_KEY` را با کلید مرحلهٔ ۱ عوض کنید:

```bash
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --role=remote --edge-address=IRAN_IP --peer-key=IRAN_KEY
```

این‌جا هم در پایان یک **کلید عمومی** چاپ می‌شود («کلید خارج»). کپی‌اش کنید
(یا `cat /etc/tuunel/node.pub`).

### مرحلهٔ ۳ — معرفی سرور خارج به سرور ایران

دوباره روی سرور **ایران** بزنید؛ `KHAREJ_KEY` همان کلید مرحلهٔ ۲ است:

```bash
curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh | sudo bash -s -- --peer-key=KHAREJ_KEY
```

### مرحلهٔ ۴ — بررسی اتصال

روی هر دو سرور:

```bash
sudo tunnelctl status          # باید وضعیت UP و پروتکل فعال را نشان دهد
```

از سرور ایران به خارج و برعکس:

```bash
ping -c 4 10.200.0.2           # روی سرور ایران
ping -c 4 10.200.0.1           # روی سرور خارج
```

اگر وصل نشد:

```bash
sudo tunnelctl doctor          # عیب‌یابی خودکار (کلید، پورت، فایروال، MTU، ...)
sudo journalctl -u tuunel -n 50 --no-pager
```

تمام! تونل بعد از ری‌استارت سرور هم خودکار بالا می‌آید.

---

## استفاده: فوروارد پورت از ایران به خارج

برای این‌که کاربر به **سرور ایران** وصل شود و ترافیک از **سرور خارج** خارج شود
(مثلاً سرویس/پنلی که روی سرور خارج روی پورت `8080` اجرا می‌شود):

روی سرور **ایران** فایل `/etc/tuunel/config.yaml` را باز کنید و بخش `forwarding` را این‌طور کنید:

```yaml
forwarding:
  tcp:
    - {listen: "0.0.0.0:8080", target: "10.200.0.2:8080"}
  udp:
    - {listen: "0.0.0.0:8080", target: "10.200.0.2:8080"}
```

سپس:

```bash
sudo systemctl reload tuunel
```

حالا هر اتصال به `IRAN_IP:8080` به پورت `8080` سرور خارج می‌رسد. می‌توانید چند خط برای چند پورت بنویسید.
(پورت `8080` را در فایروال سرور ایران هم باز کنید.)

---

## دستورات کاربردی

| کار | دستور |
|---|---|
| وضعیت تونل | `sudo tunnelctl status` |
| عیب‌یابی | `sudo tunnelctl doctor` |
| تست همهٔ پروتکل‌ها | `sudo tunnelctl test-carriers` |
| لاگ زنده | `sudo journalctl -u tuunel -f` |
| ری‌استارت | `sudo systemctl restart tuunel` |
| اعمال تغییرات فوروارد بدون قطعی | `sudo systemctl reload tuunel` |
| نسخه | `tuunel version` |
| آپدیت به آخرین نسخه (تنظیمات حفظ می‌شود) | `curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh \| sudo bash` |
| حذف (تنظیمات و کلیدها می‌ماند) | `curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh \| sudo bash -s -- --uninstall` |
| حذف کامل | `curl -fsSL https://github.com/salehsayyadi/tuunel/releases/latest/download/install.sh \| sudo bash -s -- --uninstall --purge` |

## اگر سرور ایران به گیت‌هاب دسترسی ندارد

فایل را روی سرور خارج (یا کامپیوتر خودتان) دانلود کنید و به سرور ایران بفرستید:

```bash
# روی سرور خارج
curl -fsSLO https://github.com/salehsayyadi/tuunel/releases/latest/download/tuunel-linux-amd64.tar.gz
scp tuunel-linux-amd64.tar.gz root@IRAN_IP:/root/

# روی سرور ایران
cd /root && tar xzf tuunel-linux-amd64.tar.gz && cd tuunel-v*
sudo bash install.sh --role=edge
```

(برای سرور ARM به جای `amd64` بنویسید `arm64`. مرحلهٔ ۳ هم با همین روش:
`sudo bash install.sh --peer-key=KHAREJ_KEY` داخل همان پوشه.)

## مشکلات رایج

| مشکل | راه‌حل |
|---|---|
| `/dev/net/tun missing` | TUN/TAP را از پنل VPS فعال کنید |
| وضعیت سرور خارج روی `CONNECTING` می‌ماند | پورت‌های سرور ایران باز نیست (فایروال یا فایروال دیتاسنتر) یا آی‌پی ایران اشتباه است |
| `handshake timeout` | کلیدها جابه‌جا/اشتباه وارد شده‌اند؛ مرحلهٔ ۲ و ۳ را با کلید درست تکرار کنید |
| `address already in use` روی 443 | پورت گرفته شده؛ با `--ports=...` و `--force-config` روی **هر دو** سرور نصب کنید |
| سرعت پایین روی اینترنت پرقطعی | تونل خودکار پروتکل بهتر را انتخاب می‌کند؛ وضعیت را با `tunnelctl status` ببینید |

---

مستندات فنی (انگلیسی): [docs/README.en.md](docs/README.en.md)،
[نصب](docs/INSTALL.md)، [عیب‌یابی](docs/TROUBLESHOOTING.md)، [امنیت](docs/SECURITY.md)،
[گزارش تست](docs/FINAL_VALIDATION.md). مجوز: [LICENSE](LICENSE).

</div>
