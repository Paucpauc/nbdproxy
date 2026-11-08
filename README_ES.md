# nbdproxy

[English](README.md) | [Русский](README_RU.md) | [简体中文](README_CN.md) | Español | [Français](README_FR.md)

Un pequeño proxy NBD sin dependencias externas que mantiene **abierto el socket del cliente local** mientras se reconecta a un servidor reiniciado. Admite TCP y sockets Unix en ambos extremos. El `nbd-client` de Linux no necesita `-persist` ni soporte de reconexión por netlink, incluidas versiones como la 3.26.1.

```text
Linux /dev/nbd0 ── stable local socket ── nbdproxy ── reconnect ── NBD server
```

Diseñado para un servidor local como s3nbd que se reinicia independientemente del proxy. Cada proceso ofrece una exportación configurada a **una sola conexión de cliente**. Las solicitudes se ejecutan en el orden de recepción, con un máximo de una solicitud pendiente al servidor. Se sacrifica rendimiento para simplificar el orden de repetición.

## Compilar y ejecutar

Requiere Go 1.24 o posterior; sin módulos externos ni bibliotecas de C.

```sh
go build -trimpath -o nbdproxy .
./nbdproxy -version
sudo install -d -m 0755 /run/nbdproxy
sudo ./nbdproxy \
  -listen unix:/run/nbdproxy/nbd.sock \
  -upstream tcp:127.0.0.1:10809 \
  -export disk1
```

En otra terminal de Linux:

```sh
sudo modprobe nbd
sudo nbd-client -unix /run/nbdproxy/nbd.sock /dev/nbd0 -N disk1 -b 4096
```

Use **una sola conexión**, sin `-C 2`. `-persist` no es necesario para reinicios del servidor. Evite un tiempo límite finito de E/S NBD en el kernel si las interrupciones pueden superarlo: el vencimiento de una solicitud del kernel puede hacer fallar la E/S aunque el socket local siga abierto. En un dispositivo existente, consulte `/sys/block/nbd0/queue/io_timeout` (milisegundos, si el kernel lo expone). Un valor cero de `nbd-client -timeout` conserva el ajuste actual o predeterminado del kernel; no permite restablecer de forma fiable un tiempo límite configurado previamente en un dispositivo reutilizado. Verifique el ajuste efectivo del kernel.

Ejemplo con interfaz de cliente TCP y servidor Unix:

```sh
./nbdproxy -listen tcp:127.0.0.1:10810 -upstream unix:/run/s3nbd/nbd.sock -export disk1
sudo nbd-client 127.0.0.1 10810 /dev/nbd0 -N disk1 -b 4096
```

Ningún extremo utiliza autenticación ni TLS. Limite los puertos TCP a la interfaz de bucle local o a una red de confianza restringida. El socket Unix del cliente se crea con permisos 0600. El directorio padre debe existir. Al iniciar no se eliminan automáticamente las rutas de sockets existentes; investigue y elimine manualmente los sockets obsoletos si es necesario. Go elimina el socket del listener Unix al cerrarlo normalmente.

## Qué sobrevive a un reinicio

1. El proxy negocia la exportación configurada con el servidor antes de completar la negociación del cliente. Si el servidor no está disponible al conectar por primera vez, la negociación se retrasa; la conexión inicial de `nbd-client` aún puede agotar su tiempo límite y requerir otro intento.
2. Durante la transmisión, cada solicitud y sus datos WRITE permanecen en memoria hasta recibir una respuesta completa del servidor. Las respuestas READ parciales se descartan.
3. Ante errores de conexión, negociación, transporte o tiempo límite de E/S del servidor, el proxy espera y se reconecta indefinidamente. El socket del cliente local sigue abierto. Las respuestas de error NBD del servidor se transmiten normalmente, sin reintentarlas.
4. Al reconectar, el tamaño y las banderas de transmisión del servidor deben coincidir exactamente con los valores originales. Una diferencia termina la sesión del cliente para evitar repetir solicitudes en una exportación incompatible. La dirección y el nombre configurados deben seguir apuntando al **mismo almacenamiento**: NBD no exige un UUID de exportación persistente, por lo que comprobar tamaño, banderas y nombre no detecta la sustitución por otro disco del mismo tamaño.
5. La solicitud pendiente se repite antes de enviar cualquier solicitud posterior.

La repetición garantiza **al menos una vez**, no exactamente una vez. Una WRITE puede haberse completado antes de perderse su respuesta. Repetirla solo es seguro si la operación anterior del servidor ya no puede ejecutarse más tarde, después de operaciones nuevas. Un proceso de servidor detenido cumple esta condición; las particiones de red arbitrarias, los tiempos de espera de un servidor activo o bloqueado y las escrituras concurrentes requieren garantías de aislamiento (fencing) o serialización del servidor. No lo utilice como sistema genérico de conmutación distribuida ante fallos ni junto con otro escritor.

No hay diario ni caché de escritura persistente. Un servidor que confirma escrituras volátiles puede perderlas al fallar; el proxy no puede recuperar solicitudes que ya confirmó al kernel. La durabilidad de FLUSH/FUA del servidor sigue siendo importante. La repetición también exige que WRITE/TRIM del servidor puedan repetirse de forma segura. Reiniciar o terminar **el propio proxy** rompe la conexión del cliente y queda fuera de la reconexión al servidor.

## Protocolo y límites de recursos

- NBD fixed-newstyle; EXPORT_NAME, INFO, GO, LIST y ABORT en el lado del cliente.
- READ, WRITE, FLUSH, TRIM y FUA negociado; solo respuestas simples.
- La negociación con el servidor usa EXPORT_NAME con el nombre configurado. El servidor debe admitir este modo y sus restricciones implícitas de tamaño de bloque (mínimo 1 byte).
- Las opciones no compatibles (incluidas las respuestas estructuradas y TLS) reciben ERR_UNSUP; las órdenes o banderas no compatibles reciben EINVAL. Las exportaciones de solo lectura rechazan escrituras.
- La capacidad de múltiples conexiones se oculta aunque el servidor la anuncie. Una segunda conexión de cliente se cierra sin negociación.
- Límite READ/WRITE: 32 MiB por solicitud. Una solicitud en ejecución más una de lectura anticipada; los búferes TCP/socket proporcionan contrapresión. Los búferes de datos de la aplicación se limitan a unos 96 MiB por sesión activa (incluida una respuesta de lectura), independientemente de la duración de la interrupción. Los rangos TRIM no transportan datos.
- Una firma inválida, opciones o datos WRITE demasiado grandes, o la desconexión del cliente terminan la sesión. Un cliente que nunca termine de enviar una solicitud puede ocupar la única sesión; es un servicio local de confianza, no un punto de acceso público multiusuario.
- Los fallos del servidor se registran en stderr mediante el registrador estructurado de Go. Una negociación o respuesta del servidor persistentemente inválida se reintenta; revise los registros para diagnosticarla.

Ejecute `./nbdproxy -help` para ver las opciones:

| Opción | Valor predeterminado | Significado |
|---|---|---|
| `-listen` | `unix:/run/nbdproxy/nbd.sock` | Dirección de escucha local |
| `-upstream` | `tcp:127.0.0.1:10809` | Dirección del servidor |
| `-export` | vacío | Nombre de exportación en ambos extremos |
| `-retry` | `1s` | Pausa fija entre intentos de reconexión |
| `-connect-timeout` | `5s` | Tiempo límite de conexión al servidor |
| `-handshake-timeout` | `30s` | Tiempo límite de negociación por conexión |
| `-io-timeout` | `1m` | Plazo de solicitud al servidor y de escritura de respuesta al cliente |

El plazo de la solicitud al servidor activa la reconexión; no limita el tiempo total de recuperación. El proxy no aplica un plazo de lectura al cliente durante la transmisión, por lo que los clientes inactivos siguen conectados. SIGINT/SIGTERM cierra el listener y la sesión activa, cancela los reintentos y termina. Desmonte los sistemas de archivos y desconecte NBD antes de detener el proxy.

## systemd

Instale el binario, edite upstream/export en la unidad incluida y actívela:

```sh
sudo install -m 0755 nbdproxy /usr/local/bin/nbdproxy
sudo install -m 0644 packaging/nbdproxy.service /etc/systemd/system/nbdproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now nbdproxy
sudo journalctl -u nbdproxy -f
```

La unidad usa DynamicUser, crea `/run/nbdproxy` y no necesita privilegios de root. Root puede conectar `nbd-client` a su socket con permisos 0600. Un servidor Unix debe permitir acceso al usuario del servicio; en ese caso, sustituya DynamicUser por un usuario/grupo fijo y configure los permisos del socket. Mantenga esta unidad independiente del servicio del servidor: no añada PartOf/BindsTo que detengan el proxy al reiniciar el servidor. Restart=on-failure reinicia el proceso, pero no conserva las sesiones NBD existentes.

## Validación

```sh
go vet ./...
go test -race ./...
go test -run='^$' -fuzz=FuzzExportName -fuzztime=10s
```

Las pruebas cubren escrituras completadas con respuestas perdidas, respuestas de lectura parciales, orden de flush, cambios de exportación, cancelación, negociación malformada, validación de solicitudes y fuzzing del analizador. CI ejecuta pruebas en Linux y macOS y compila todos los destinos de publicación. El trabajo Linux también conecta un `/dev/nbd0` real mediante el nbd-client de la distribución, realiza E/S directa, detiene el servidor, comprueba que una E/S permanece pendiente, reinicia el servidor y verifica los datos y las escrituras/flush posteriores.

Para ejecutar manualmente esta prueba de integración que sobrescribe el dispositivo, use **un host Linux desechable con `/dev/nbd0` sin utilizar** e instale nbd-client:

```sh
sudo modprobe nbd nbds_max=4 max_part=0
go test -c -o /tmp/nbdproxy.test
sudo env NBDPROXY_KERNEL_TEST=1 /tmp/nbdproxy.test \
  -test.v -test.run '^TestLinuxKernelRestart$' -test.timeout 60s
```

La prueba escribe en `/dev/nbd0`. Solo se ejecuta si se activa explícitamente y se omite en las pruebas habituales. La prueba con el kernel real no puede ejecutarse en macOS.

## Publicaciones

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.0
./scripts/release.sh v0.1.0
```

Genera archivos para Linux amd64/arm64 y macOS amd64/arm64, cada uno con el binario autónomo, documentación, licencia y unidad systemd, paquetes DEB y RPM para Linux amd64/arm64 y sumas SHA-256 de todos los artefactos en `dist/`.

Después de subir este repositorio a GitHub, enviar una etiqueta `v*` activa el flujo de publicación. Ejecuta vet y las pruebas, construye los archivos y paquetes y publica inmediatamente una GitHub Release con notas generadas automáticamente. Los flujos de CI y publicación solo tienen permisos de lectura, salvo contents:write en el trabajo de publicación.

Referencia del protocolo: [especificación NBD](https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md).
Licencia: MIT.
