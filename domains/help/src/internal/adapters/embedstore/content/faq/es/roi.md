## Para qué sirve esta pestaña
ROI y ahorro muestra cuánto dejó de costar el gateway y por qué, separando por fuerza de prueba: ahorro **comprobado** (cache — el mismo modelo costó menos, y eso es observable) y **contrafactual** (servimos un modelo distinto del pedido y comparamos con lo que habría costado el pedido).

## Cómo usarla
Las tarjetas suman el ahorro por mecanismo (cache, auto-cheapest, fallback, degradación por budget). Los gráficos muestran cada mecanismo a lo largo del tiempo y el consolidado. "De qué bolsillo salió" separa el crédito del proveedor (saldo quemado) del ahorro real — el crédito no es ahorro.

## Preguntas frecuentes
- **¿Cuál ahorro puedo tomar en serio?** El comprobado (cache) no depende de ninguna suposición. El contrafactual es real en dinero, pero solo vale si el modelo pedido era la intención real.
- **¿Por qué el ahorro parece alto?** Si el precio es de lista y tienes descuento por contrato, informa tu precio en Modelos y enrutamiento — si no, costo y ahorro quedan por encima de lo real.
