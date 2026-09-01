## Para qué sirve esta pestaña
API Keys emite y revoca las claves del gateway. Cada clave resuelve **org + equipo + app**: es lo que separa tu costo por equipo y por app. Guardamos solo el hash — el valor aparece una sola vez, al crearla.

## Cómo usarla
Elige un equipo y una app existentes (creados en Equipos y apps) y emite. Copia la clave en el momento — no la mostramos de nuevo. Apunta el `base_url` de tu app al gateway y usa la clave como Bearer token.

## Preguntas frecuentes
- **No puedo crear equipo ni app aquí.** Por diseño: la creación vive en Equipos y apps; aquí solo asocias la clave a algo que ya existe.
- **¿La clave es "de equipo" o "de app"?** La clave siempre lleva el equipo; la app puede ser una específica o `default` (todo el equipo).
- **Perdí la clave.** No se puede recuperar (solo guardamos el hash). Revócala y emite otra.
